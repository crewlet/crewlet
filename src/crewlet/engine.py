"""Engine — the central entry point that wires everything together."""

from __future__ import annotations

import asyncio
import contextlib
import logging
import os
import signal
import sys
import time
from collections.abc import Awaitable, Callable
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from crewlet.a2a.service import A2AService

from crewlet._logging import configure_logging, get_logger
from crewlet._tasks import cancel_and_wait
from crewlet.agent.instance import AgentInstance, AgentState
from crewlet.agent.pool import AgentPool
from crewlet.agent.turn import TurnEngine
from crewlet.budget_reporter import BudgetReporter
from crewlet.concurrency import BudgetManager, ConcurrencyController
from crewlet.config import (
    MCPServerConfig,
    config_to_organization,
    parse_mcp_servers,
    register_github_accounts_from_org,
    register_gitlab_accounts_from_org,
    register_jira_accounts_from_org,
    register_mattermost_bots_from_org,
    register_plane_accounts_from_org,
    register_slack_apps_from_org,
    resolve_env_vars,
    resolve_node_id,
)
from crewlet.config_resolution import resolution_fingerprint
from crewlet.db.protocol import StorageBackend
from crewlet.events.types import (
    Event,
    OrgStarted,
    OrgStopped,
)
from crewlet.extensions.loader import ExtensionManager
from crewlet.extensions.protocol import Extension, ExtensionContext
from crewlet.mcp.bridge import MCPToolBridge, mcp_instance_name
from crewlet.notifications.coalesce import coalesce_notifications, conversation_key
from crewlet.notifications.protocol import Transport
from crewlet.notifications.service import NotificationService
from crewlet.observability import ObservabilityManager
from crewlet.org.models import Organization
from crewlet.providers.llm.protocol import LLMProvider
from crewlet.queue.protocol import BatchOptions, DeferDelivery, EventQueue
from crewlet.queue.topics import (
    agent_control_group,
    agent_control_topic,
    agent_inbox_group,
    agent_inbox_topic,
)
from crewlet.secrets.resolver import refresh_secret_snapshot
from crewlet.task.delegation import DelegationHandler
from crewlet.task.tracker import ExecutionTracker
from crewlet.tools.builtin import register_builtin_tools
from crewlet.tools.colleague import register_colleague_tools
from crewlet.tools.protocol import Tool
from crewlet.tools.registry import ToolRegistry
from crewlet.tools.run_sandbox_tool import register_run_sandbox_tool
from crewlet.tools.spawn_subagent_tool import register_spawn_subagent_tool
from crewlet.work_key import bind_work_key, derive_work_key

logger = get_logger("engine")

# Why a seat's inbox is held while this node has no turn engine.
#
# Its OWN reason. Pause holds are reason-scoped precisely so one
# subsystem cannot un-gate another's, and three independent things gate
# an agent inbox: this park, the sandbox busy gate, and the
# config-divergence shed. Sharing the default with the sandbox gate made
# the late turn-engine build's blanket resume release a live run's hold
# — delivering messages to an agent whose turn is suspended mid-run.
_NO_TURN_ENGINE_PAUSE_REASON = "no_turn_engine"

# Inbox trigger types the completion ledger covers.
#
# These are the types that run a TURN, which is the only work whose
# duplication costs anything outward-facing. The informational types
# (``task_created`` / ``task_completed`` / ``task_delegated``) are logged
# and dropped, so recording them would be bookkeeping about nothing.
#
# ``a2a_request`` / ``a2a_message`` were exempt while the content rode a
# process-local ``asyncio.Queue``: ``_handle_a2a`` drained it
# DESTRUCTIVELY, so a re-run on any node — including this one — found an
# empty channel and told the agent nobody had sent anything. Neither
# branch of a short-circuit-or-re-run choice could be honoured, so the
# ledger stayed out of it. The content rides the durable wake event now,
# which makes an A2A trigger re-runnable and therefore ledgerable like
# any other.
#
# The requester's hop NEEDS it. The responder's is guarded twice over —
# it answers and closes, and a closed channel refuses a second answer —
# but the hop that carries the reply BACK lands on a channel that is
# already closed by design, so the ledger is the only thing standing
# between a redelivery and a second turn spent acting on the same answer.
_LEDGERED_INBOX_TYPES = frozenset(
    {
        "task_assigned",
        "notification",
        "external_notification",
        "a2a_request",
        "a2a_message",
    }
)

# Type alias for event handler callbacks (replaces old EventBus.EventHandler).
_EventHandler = Callable[[Event], Awaitable[None]]


def _parse_otlp_headers(raw: str) -> dict[str, str]:
    """Parse the ``OTEL_EXPORTER_OTLP_HEADERS`` ``k=v,k2=v2`` form into a dict.

    Used to give the sandbox OTLP receiver the engine's upstream backend
    auth (added engine-side, never handed to the sandbox).
    """
    headers: dict[str, str] = {}
    for pair in raw.split(","):
        key, sep, value = pair.partition("=")
        if sep and key.strip():
            headers[key.strip()] = value.strip()
    return headers


def _scheduled_deadline(event: Event) -> float | None:
    """Wall-clock cap (seconds) for a scheduled ``TaskAssigned``, else ``None``.

    Scheduler-originated tasks carry ``scheduled=True`` and a
    ``timeout_seconds`` in their payload; the turn engine enforces the
    cap.  Non-scheduled tasks return ``None`` (no cap).
    """
    payload = getattr(event, "payload", None) or {}
    if not payload.get("scheduled"):
        return None
    raw = payload.get("timeout_seconds")
    try:
        deadline = float(raw)
    except (TypeError, ValueError):
        return None
    return deadline if deadline > 0 else None


# Bounded retry for the Tool Skills full registry walk
# (``_kick_tool_skill_resync``).  Sized against the compose boot race —
# the ordinary failure mode is the engine and the knowledge backend
# coming up together, with the backend not yet accepting connections
# when the boot walk fires.  5 attempts with exponential backoff from
# 5 s (5 + 10 + 20 + 40 = 75 s of waiting) comfortably covers a Plane /
# Confluence container's API becoming ready (tens of seconds on the
# reference compose stack) without retrying forever against a genuinely
# misconfigured backend; the transport's project-cache floor does not
# block in-window retries because a FAILED fetch does not burn it.
# After the last attempt the walk logs ``tool_skill_resync_exhausted``
# loudly and the registry keeps whatever it currently holds (on a
# backend cut-over that means the OLD backend's skills — better than an
# empty prompt surface, but the operator is told explicitly).
_TOOL_SKILL_RESYNC_ATTEMPTS = 5
_TOOL_SKILL_RESYNC_BASE_DELAY_SECONDS = 5.0

# Toolsets that crewlet requires for atlassian MCP servers.
# ``jira_users`` exposes ``jira_get_user_profile`` which is needed to
# resolve per-agent Jira account IDs for webhook routing.
_ATLASSIAN_REQUIRED_TOOLSETS = {"jira_users"}


def _ensure_atlassian_toolsets(server_name: str, env: dict[str, str]) -> None:
    """Ensure required toolsets are enabled for atlassian MCP servers.

    mcp-atlassian gates tools behind the ``TOOLSETS`` env var.
    Some toolsets that crewlet relies on (e.g. ``jira_users`` for
    ``jira_get_user_profile``) are *not* in the default set.  This
    helper appends them when missing.
    """
    if "atlassian" not in server_name:
        return

    existing = env.get("TOOLSETS", "")
    tokens = {t.strip() for t in existing.split(",") if t.strip()}
    missing = _ATLASSIAN_REQUIRED_TOOLSETS - tokens
    if not missing:
        return

    # If TOOLSETS was unset the server defaults to *all* toolsets
    # (with a deprecation warning).  Set it to "all" plus the
    # required ones explicitly so it keeps working when
    # mcp-atlassian changes the default in v0.22+.
    if not tokens:
        tokens = {"all"}
    tokens |= missing
    env["TOOLSETS"] = ",".join(sorted(tokens))


def _index_extension_entries(
    entries: list[dict[str, Any]],
) -> dict[str, dict[str, Any]]:
    """Map each ``{"name": {settings}}`` entry to its settings dict.

    Used by ``_apply_extensions_live`` to compare per-extension config
    so that editing one extension doesn't restart its neighbours.
    Malformed entries (not a single-key dict) are silently dropped —
    they would have been rejected by ``parse_extensions`` upstream.
    """
    out: dict[str, dict[str, Any]] = {}
    for entry in entries:
        if isinstance(entry, dict) and len(entry) == 1:
            ((name, settings),) = entry.items()
            out[name] = settings if isinstance(settings, dict) else {}
    return out


def _print_signal_notice(message: str) -> None:
    """Best-effort operator feedback from inside a signal handler.

    stderr can be a broken pipe at exactly this moment: the terminal
    delivers Ctrl+C to the whole foreground process group, so in
    ``crewlet run 2>&1 | tee out`` the first press also kills ``tee``
    and every write to stderr from then on raises ``BrokenPipeError``.
    An exception escaping a Python signal handler is re-raised inside
    whatever frame the main thread happened to be executing — it can
    silently kill an arbitrary task (the engine then runs on, blind,
    looking wedged) or rip down the event loop mid-step (leaving e.g.
    the Pulsar client's C++ threads alive at interpreter exit, where
    their callback into the Python logging bridge aborts the process).
    The notice is optional; the shutdown is not — swallow everything.
    """
    with contextlib.suppress(Exception):
        print(message, file=sys.stderr, flush=True)


def _handle_shutdown_signal(
    count: int,
    signum: int,
    *,
    schedule_graceful: Callable[[], None],
    schedule_force_cancel: Callable[[], None],
    hard_exit: Callable[[int], Any] | None = None,
) -> None:
    """One press of the SIGINT/SIGTERM escalation ladder (see
    :meth:`Engine.run`).

    Runs inside a signal handler, so two invariants hold:

    - The shutdown action is scheduled BEFORE the console notice, so a
      dead stderr can never stop the shutdown from starting.
    - Nothing in here may raise (see :func:`_print_signal_notice` for
      why an escaping exception is dangerous); the schedule callbacks
      are expected to be exception-proof as well.
    """
    if count == 1:
        schedule_graceful()
        _print_signal_notice(
            "\n[crewlet] Graceful shutdown: waiting for in-flight "
            "agent turns to finish (Ctrl+C again to force-stop)…"
        )
    elif count == 2:
        schedule_force_cancel()
        _print_signal_notice(
            "\n[crewlet] Force stop: cancelling in-flight turns — "
            "their tasks will be redelivered on the next boot "
            "(Ctrl+C again to exit immediately)…"
        )
    else:
        _print_signal_notice(f"\n[crewlet] Hard exit on signal {signum}.")
        # Resolve ``os._exit`` at call time (not as a def-time default)
        # so tests that monkeypatch it keep working.
        (hard_exit if hard_exit is not None else os._exit)(1)


class ConfigApplyError(RuntimeError):
    """Raised when :meth:`Engine.apply_config` fails mid-apply.

    Carries the name of the subsystem that failed, the original
    exception, and the ordered list of subsystems that DID complete
    before the failure (so the dashboard can render "applied: org,
    budgets; failed at: turn_engine").  Rollback to the prior state
    has already run by the time this is raised.
    """

    def __init__(
        self,
        subsystem: str,
        original: Exception,
        applied_before_failure: list[str],
        *,
        degraded: bool = False,
    ) -> None:
        self.subsystem = subsystem
        self.original = original
        self.applied_before_failure = applied_before_failure
        self.degraded = degraded
        """Whether rollback could NOT restore what the apply tore down.

        ``_rollback`` restores and RESTARTS transports, but it cannot
        respawn the per-role MCP children the failed revision already
        started. So a failure AFTER a restart-required subsystem was
        mutated leaves the node claiming the prior epoch while its tool
        surface may be amputated. That is a materially different state
        from a clean rollback, and the control plane has to be able to
        tell them apart — a node in it must never be counted as somewhere
        work can safely go.

        Set conservatively, on *entering* the restart-required block
        rather than on proving something was torn down: a false
        ``degraded`` costs one node's readiness, a false ``ok`` costs
        silent breakage no probe would catch."""
        super().__init__(
            f"apply_config failed at subsystem={subsystem!r}: "
            f"{type(original).__name__}: {original}"
        )


class Engine:
    """The Crewlet Engine — orchestrates an AI agent company.

    Wires together all subsystems: organization model, agent pool,
    event queue, task engine, tool registry, storage, communication
    channels, and knowledge system.
    """

    # ``apply_config`` per-subsystem dispatch order — must
    # run in this sequence so each handler observes the prior
    # subsystem's state already updated.  ``org`` runs first so role
    # add/remove decisions land before per-role budgets and MCP
    # respawns; role LLM-key lookups happen at turn time (not
    # AgentDefinition construction) so ``providers`` can swap after.
    # ``restart_required`` runs last so transport / MCP / integration
    # rebuilds see the new org + provider state.
    _APPLY_DISPATCH_ORDER: tuple[str, ...] = (
        "org",
        "budgets",
        "turn_engine",
        "providers",
        "scalars",
        "restart_required",
    )

    def __init__(
        self,
        organization: Organization,
        llm_providers: dict[str, LLMProvider] | None = None,
        storage: StorageBackend | None = None,
        tools: list[Tool] | None = None,
        extensions: list[Extension] | None = None,
        mcp_servers: list[MCPServerConfig] | None = None,
        notification_transports: list[Transport] | None = None,
        notification_rate_limit: int = 0,
        notification_coalesce_window_seconds: float = 0.0,
        notification_coalesce_max_batch: int = 20,
        max_concurrent: int = 10,
        org_token_budget: int = 0,
        event_queue: EventQueue | None = None,
        github_config: Any = None,
        gitlab_config: Any = None,
        plane_config: Any = None,
        debug: bool = False,
        api_port: int = 0,
        api_host: str = "0.0.0.0",
        turn_engine_config: Any = None,
        learning_config: Any = None,
        scheduling_config: Any = None,
        embeddings: Any = None,
        lease_store: Any = None,
        turn_completion_store: Any = None,
    ) -> None:
        self.debug = debug
        self._turn_engine_config = turn_engine_config
        self._learning_config = learning_config
        self._scheduling_config = scheduling_config
        self._scheduler: Any = None
        self._budget_reporter: Any = None
        self._started_at: str = ""
        self._embeddings = embeddings
        self._episode_store: Any = None
        self._skill_variables: dict[str, str] = {}
        self._api_port = api_port
        self._api_host = api_host
        self._event_store: Any = None
        # Learning workers are wired in ``start()`` when configured;
        # set to ``None`` here so ``stop()`` is safe even when the
        # engine never made it through the spawn cascade (unconfigured
        # boot, integration tests).
        self._reflect_engine: Any = None
        self._episode_lifecycle_worker: Any = None
        self._skill_curator_worker: Any = None
        self._maintenance_worker: Any = None
        # In-flight Tool Skills registry walk (boot populate or
        # live-refresh re-seed).  Tracked so a re-kick supersedes a
        # still-retrying older walk instead of racing it.
        self._tool_skill_resync_task: asyncio.Task[None] | None = None
        # ``True`` once the Tier B spawn cascade in ``start()`` has
        # completed (agents spawned, MCP servers up, transports wired).
        # Diff handlers gate live-rewire branches on this so a config
        # change before the cascade just updates stored configs — the
        # cascade reads them when it runs.  Initialised here (not a
        # late ``setattr`` after spawn) so every read site can use a
        # direct attribute access instead of defensive ``getattr``.
        self._tier_b_done: bool = False
        # Tier B revision currently applied (None = unconfigured state).
        # Populated by ``apply_config``; consulted by ``start`` to gate
        # the spawn cascade and by ``/health`` / dashboard for the
        # ``configured`` predicate.
        self._active_config: Any = None
        # Fingerprint of what the active config's ${VAR} references
        # resolved to when it was applied. Half of the no-op check —
        # see crewlet.config_resolution.
        self._active_resolution: str = ""
        # Control-plane state — see crewlet.db.config_plane.
        self._config_plane_store: Any = None
        self._node_id: str = ""
        # What this node is willing to do, and how it is labelled.
        # Resolved from Tier A at ``start``; the all-roles default until
        # then, which is what an engine constructed directly (a test, an
        # embedding) means by saying nothing.
        self._node_profile: Any = None
        self._incarnation: str = ""
        self._applied_epoch: int = 0
        self._apply_attempts: int = 0
        # Which epoch ``_apply_attempts`` is counting. The budget is PER
        # REVISION: without this it is a per-process budget, and a node
        # that exhausts it on one bad revision never attempts any later
        # one — including the fixed revision the operator pushes as the
        # documented remedy.
        self._apply_attempts_epoch: int = 0
        self._ticks_behind: int = 0
        self._apply_status: Any = None
        # What the last recorded status was ABOUT, so the per-tick
        # heartbeat can re-assert it verbatim rather than inventing one.
        self._applied_revision_id: Any = None
        self._apply_error: str = ""
        self._posture: Any = None
        self._reconcile_task: asyncio.Task[Any] | None = None
        # Set by the broadcast activation nudge to shorten the next poll.
        # Never carries data — see ``_subscribe_activation_nudge``.
        self._reconcile_nudge: asyncio.Event = asyncio.Event()
        self._nudge_unsubscribe: Any = None
        # Refreshes the embedded API's cached projection (webhook
        # secrets, roles, org) from the activation pointer.  Driven from
        # this engine's reconcile tick rather than its own loop, so a
        # merged node polls the pointer once, not twice.
        self._api_refresher: Any = None
        # Secret-encryption keyring (Tier A ``secrets``).  ``None`` when
        # encryption is disabled; set by ``from_bootstrap``.  Used by
        # ``load_config`` to decrypt the whole config document in an
        # incoming revision before the builders construct providers /
        # transports / per-role MCP.  See
        # ``docs/concepts/configuration.md`` § Secrets.
        self._cipher: Any = None
        # Serialises ``apply_config`` invocations (CLI path, Pulsar
        # handler, tests).  Set here so first-activation has a lock
        # to take even before any apply runs.
        self._apply_lock: asyncio.Lock = asyncio.Lock()
        if debug:
            configure_logging(level=logging.DEBUG)
            logger.debug("debug_mode_enabled")

        logger.info("engine_initializing", org=organization.name)
        self.org = organization
        self._llm_providers = llm_providers or {}
        # Sandboxed Execute backend state — populated on
        # first ``apply_config`` from ``providers.sandbox`` / ``providers.llm``.
        # ``None`` manager = sandbox backend disabled.
        self._sandbox_manager: Any = None
        self._sandbox_pending_store: Any = None
        self._sandbox_coordinator: Any = None
        self._sandbox_waiter: Any = None
        self._sandbox_otel_receiver: Any = None
        self._llm_provider_configs: dict[str, Any] = {}
        self.storage = storage
        self._running = False
        self._stop_step_timeout = 10.0
        # Flipped at the top of ``stop()`` / ``_force_stop()`` so the
        # dashboard's ``/health`` reports the drain while it is
        # happening (``is_running`` only flips once teardown is done).
        self._shutting_down = False
        # How often the graceful drain logs ``drain_in_progress`` with
        # the in-flight count.  Attribute (not a module constant) so
        # tests can shrink it.
        self._drain_log_interval = 10.0
        # Embedded API server + its serve task (``_start_embedded_api``).
        # Kept so shutdown can keep the dashboard alive through the
        # drain and then bring it down cleanly.
        self._api_server: Any = None
        self._api_serve_task: asyncio.Task[None] | None = None
        # Escalation timeouts for ``_stop_embedded_api``: graceful
        # (``should_exit``) wait, then forced (``force_exit``) wait,
        # then the serve task is cancelled outright.
        self._api_stop_graceful_timeout = 5.0
        self._api_stop_force_timeout = 3.0

        # Initialize subsystems — memory fallbacks for tests only.
        # Production code (from_config / cli.py) always passes real impls.
        logger.debug("init_subsystem", subsystem="event_queue")
        if event_queue is None:
            from crewlet.queue.memory import MemoryEventQueue

            event_queue = MemoryEventQueue()
        self.event_queue: EventQueue = event_queue

        logger.debug("init_subsystem", subsystem="agent_pool")
        self.agent_pool = AgentPool(self.event_queue)
        logger.debug("init_subsystem", subsystem="tool_registry")
        self.tool_registry = ToolRegistry()
        logger.debug("init_subsystem", subsystem="execution_tracker")
        self.execution_tracker = ExecutionTracker()
        logger.debug("init_subsystem", subsystem="delegation")
        self.delegation = DelegationHandler(
            organization, self.agent_pool, self.event_queue
        )
        self.turn_engine: TurnEngine | None = None
        self._extension_manager = ExtensionManager()
        self._pending_extensions = list(extensions or [])
        # Observability hooks registered before ``start`` — see
        # ``_subscribe_hook``.  A subscription needs a live broker
        # connection, so they cannot be attached at registration time.
        self._pending_hooks: list[tuple[str, str, _EventHandler]] = []
        self._queue_started = False
        # Seat placement. Constructed in ``start`` once the storage
        # backend is known — see ``_build_seat_host``.
        self._seat_host: Any = None
        self._seat_host_started = False
        # The event-loop watchdog. Lives and dies with the seat host,
        # because what it protects is the seat host's promise: leases
        # lapse on their own when the loop stalls, but the broker session
        # does NOT — the client's IO threads keep answering keepalives,
        # so the broker holds this node's prefetch for the full ack
        # timeout while a peer runs the seats. Leaving collapses that.
        self._loop_watchdog: Any = None
        # An injected placement backend, for the caller that runs more
        # than one engine against one fleet.  Left ``None``,
        # ``_build_seat_host`` derives it from ``storage`` — the right
        # default, because a process-local store is only ever correct
        # for the process that is the whole fleet.
        self._lease_store: Any = lease_store
        # The completion ledger — "has this trigger already been worked?"
        # Derived from ``storage`` at start; injectable for the same
        # reason the lease store is, and it must be the SAME object
        # across a fleet or it deduplicates nothing across a takeover.
        self._turn_completions: Any = turn_completion_store

        # Tool-skill registry — populated from the knowledge base at boot
        # and via webhook events. Lives engine-wide; threaded into
        # the TurnEngine so every per-phase prompt builder can consult it.
        from crewlet.agent.skills import PromptSkillRegistry

        self._prompt_skill_registry: PromptSkillRegistry = PromptSkillRegistry()

        # Concurrency & observability
        logger.debug(
            "init_subsystem", subsystem="concurrency", max_concurrent=max_concurrent
        )
        self.concurrency = ConcurrencyController(max_concurrent=max_concurrent)
        self.budget_manager = BudgetManager(org_budget=org_token_budget)
        # Reads the manager through a callable rather than capturing it:
        # ``_apply_config`` replaces ``budget_manager`` wholesale on the
        # first activation, and a captured reference would leave the
        # reporter publishing a meter nobody charges any more.
        self._budget_reporter = BudgetReporter(
            manager_of=lambda: self.budget_manager,
            event_queue=self.event_queue,
            role_of=self._role_for_agent_id,
        )
        logger.debug("init_subsystem", subsystem="observability")
        self.observability = ObservabilityManager(self.event_queue)

        # A2A service. Always built: it needs no bus any more, only the
        # durable queue every engine already has. It used to be gated on
        # an ``a2a_bus`` argument, so an engine constructed without one
        # had ``a2a_ask`` fail with "A2A service not available" — a
        # wiring detail the agent experienced as a missing capability.
        from crewlet.a2a.service import A2AService

        self.a2a_service: A2AService | None = A2AService(queue=self.event_queue)

        # Background tasks / cleanup
        self._cancel_deadline_timers: Callable[[], None] | None = None

        # MCP server configs (launched during start())
        self._mcp_configs = mcp_servers or []
        self.mcp_bridge: MCPToolBridge | None = None
        # Per-role MCP tools (from per-role server instances)
        self._role_mcp_tools: dict[str, list[Any]] = {}

        # GitHub integration (optional)
        self._github_config = github_config

        # GitLab integration (optional)
        self._gitlab_config = gitlab_config

        # Plane integration (optional)
        self._plane_config = plane_config

        # Notification service (started during start())
        # Transports passed in programmatically (custom transports — see
        # docs/integrations/custom-transports.md). Kept separate from the
        # config-derived ones so a config activation, which rebuilds those,
        # cannot drop them.
        self._custom_transports = list(notification_transports or [])
        self._pending_transports = list(self._custom_transports)
        self._notification_rate_limit = notification_rate_limit
        self.notification_service: NotificationService | None = None
        self.handle_registry: Any = None

        # Inbox batching (see docs/concepts/event-system.md § Inbox batching).
        # One shared mutable BatchOptions for every agent-inbox
        # subscription: the queue's batch consume loops read it each
        # cycle, so live config reloads take effect by mutating the
        # fields in place — no re-subscription.
        self._inbox_batch_options = BatchOptions(
            linger_seconds=notification_coalesce_window_seconds,
            max_batch=notification_coalesce_max_batch,
        )
        # Agent handles whose inbox is already subscribed. Subscription must
        # be IDEMPOTENT: boot subscribes every agent, and the late
        # turn-engine path (_ensure_turn_engine_after_providers) walks the
        # pool again — without this guard each agent got TWO competing
        # consumers in one process, so two of its events could dispatch
        # concurrently and the loser NAK'd toward the dead-letter topic.
        self._subscribed_inboxes: dict[str, int] = {}
        """Attached seat inboxes, ``handle -> lease epoch``.

        Written and cleared ONLY by the seat hooks, so it says exactly
        what this process is consuming. It used to be a set maintained by
        whatever happened to subscribe, and its idempotence guard made a
        re-attach a silent no-op: a seat released by any path that forgot
        to discard the handle came back owned in the lease table and dark
        in the process, with the absence of a log line as the only
        signal."""
        # Fire-and-forget requeue tasks (memory-backend re-entrancy guard in
        # the inbox handler); held so they aren't garbage-collected mid-run.
        self._requeue_tasks: set[asyncio.Task[None]] = set()

        # Register built-in tools (task-management tools are not
        # registered — agents use MCP tools for external PM tools)
        register_builtin_tools(self.tool_registry)
        # Register the colleague-surface ``a2a_ask`` builtin.  The
        # cross-platform "talk to a teammate" surface is the upstream
        # MCP tools directly (slack_conversations_postMessage,
        # jira_add_comment, etc.) -- no engine-side wrapper layer.
        register_colleague_tools(self.tool_registry)
        # Register the spawn_subagent tool so Execute-phase LLMs can
        # spawn ephemeral bespoke workers.
        register_spawn_subagent_tool(self.tool_registry)
        # Register the run_sandbox tool so Execute-phase LLMs of sandbox-
        # enabled roles can run real code work in a detached sandbox and
        # continue the turn with its result.
        register_run_sandbox_tool(self.tool_registry)

        # Register custom tools
        if tools:
            logger.debug("custom_tools_registered", count=len(tools))
            for tool in tools:
                self.tool_registry.register(tool)

        logger.info(
            "engine_initialized",
            org=organization.name,
            llm_providers=len(self._llm_providers),
        )

    @classmethod
    def from_bootstrap(
        cls,
        bootstrap: Any,
        *,
        storage: StorageBackend | None = None,
        event_queue: EventQueue | None = None,
        embeddings: Any = None,
        company_config_store: Any = None,
        lease_store: Any = None,
        turn_completion_store: Any = None,
    ) -> Engine:
        """Construct an engine from Tier A bootstrap state only.

        The returned engine boots in the **unconfigured** state:
        no ``Organization``, no LLM providers, no MCP processes,
        no integrations.  Tier A surfaces (Pulsar, PostgreSQL, the
        API socket) are up.  Call :meth:`apply_config` with a
        :class:`CompanyConfig` to populate the company — either at
        boot (from the active row in ``CompanyConfigStore``) or
        live (from a Pulsar ``revision_activated`` event).

        See ``docs/concepts/configuration.md``.
        """
        from crewlet.org.models import Organization

        empty_org = Organization(name="", roles=[], units=[])
        engine = cls(
            organization=empty_org,
            storage=storage,
            event_queue=event_queue,
            embeddings=embeddings,
            debug=bootstrap.debug,
            api_port=bootstrap.api.port,
            api_host=bootstrap.api.host,
            lease_store=lease_store,
            turn_completion_store=turn_completion_store,
        )
        engine._bootstrap = bootstrap
        engine._company_config_store = company_config_store
        # Resolved here as well as in ``start()`` because the boot-time
        # apply — and the ``_seed_applied_epoch`` that follows it — runs
        # before ``start()`` does, and a node that reports its apply
        # status under an empty id is invisible to its peers.
        engine._node_id = resolve_node_id(bootstrap)
        # Build the secret-encryption keyring from Tier A ``secrets``.
        # ``None`` when no keys are configured (encryption disabled);
        # ``load_config`` then fails closed only if an activated revision
        # is actually stored as an encrypted document.
        from crewlet.secrets import KeyringCipher

        engine._cipher = KeyringCipher.from_config(bootstrap.secrets)
        # _active_config stays None — the unconfigured sentinel.
        logger.info(
            "engine_unconfigured_waiting",
            api_host=bootstrap.api.host,
            api_port=bootstrap.api.port,
        )
        return engine

    async def _complete_deferred_migrations(self, new: Any) -> None:
        """Apply migrations that were waiting on this config's embedding width.

        The pgvector migrations bake ``vector(N)`` at creation and the
        sequence is forward-only, so the migrator refuses to run them
        until a config declares the width — which means a database
        bootstrapped through the **unconfigured state** (boot the engine
        with no revision, then ``PUT /config``) reaches this point with
        ``episodes``, ``agent_diary``, ``scheduled_runs`` and
        ``pending_sandbox_run`` still absent.  That is a first-class,
        documented flow, and nothing else on it ever migrates: without
        this the engine would spawn the whole org against a partial
        schema and stay there until someone restarted it — onboarding
        lookups raising every turn, every scheduler fire failing, and
        detached sandbox runs unable to record the row they resume from.

        Idempotent, advisory-lock-serialized, and a no-op once the schema
        is complete, so it is safe on every activation.  Failure is
        logged, not raised: a config apply that otherwise succeeded must
        not be rolled back because the schema could not be advanced —
        the next activation retries.
        """
        from crewlet.db.client import Database

        if not isinstance(self.storage, Database):
            return
        try:
            from crewlet.cli import _migration_vars
            from crewlet.db.migrator import migrate

            template_vars = _migration_vars(new)
            if not template_vars:
                return  # this config declares no width either
            applied = await migrate(self.storage, template_vars=template_vars)
            if applied:
                logger.info("deferred_migrations_applied", versions=applied)
        except Exception as exc:
            logger.error("deferred_migrations_failed", error=str(exc))

    async def apply_config(self, new: Any) -> list[str]:
        """Apply a :class:`CompanyConfig` to this engine.

        Returns the ordered list of subsystem names that were touched
        (``["org", "budgets", "turn_engine", ...]``); the
        ``revision_activated`` Pulsar handler forwards this to
        ``ConfigRevisionApplied.applied_subsystems`` so the dashboard
        can render a "what changed" summary.  On first activation the
        list is the full spawn cascade.

        First-activation populates Tier-B-driven engine state from an
        empty baseline; subsequent calls compute a diff against
        ``self._active_config`` and dispatch to the per-subsystem
        handlers.

        Concurrency-safe via ``self._apply_lock``.
        """
        from crewlet.engine_builders import (
            build_embedding_provider,
            build_extensions,
            build_github_integration,
            build_gitlab_integration,
            build_llm_providers,
            build_notification_transports,
            build_plane_integration,
            build_sandbox_manager,
            resolve_forge_app_id,
            resolve_skill_variables,
        )

        async with self._apply_lock:
            # Re-read the secret store first, so this apply resolves every
            # ${VAR} against current values.  Deliberately BEFORE the
            # no-op early-out below: re-activating an unchanged revision
            # is exactly how an operator asks a running engine to pick up
            # a rotated credential, and skipping the refresh there would
            # make that gesture silently do nothing.
            await refresh_secret_snapshot()
            await self._complete_deferred_migrations(new)
            old = self._active_config
            # What this payload's ``${VAR}`` references CURRENTLY resolve
            # to.  Computed after ``refresh_secret_snapshot`` above, so a
            # value just written to the secret store is already visible.
            new_payload = new.model_dump()
            fingerprint = resolution_fingerprint(new_payload)
            if old is not None:
                # No-op early-out: a re-activation of the current
                # revision (operator clicks revert-to-current, an idle
                # PUT, etc.) would otherwise pay for the full snapshot
                # capture + every subsystem's `if old.X != new.X`
                # comparison.  Short-circuit before touching anything.
                #
                # The FINGERPRINT is half of that comparison, and the
                # half that used to be missing.  Re-activating an
                # unchanged revision is the documented way to make a
                # running engine pick up a rotated credential — and it is
                # by definition a byte-identical payload, so keying the
                # early-out on the payload alone made that gesture do
                # nothing but swap the secret snapshot.  Every subsystem
                # that captured a resolved value (an MCP child's spawn
                # env, an LLM client, a transport header) kept the old
                # one, indefinitely.
                if (
                    old.model_dump() == new_payload
                    and fingerprint == self._active_resolution
                ):
                    logger.info("apply_config_noop", org=new.name)
                    return []
                if old.model_dump() == new_payload:
                    logger.info("apply_config_rotation", org=new.name)
                    applied_rotation = await self._apply_credential_rotation(new)
                    self._active_resolution = fingerprint
                    return applied_rotation
                # Per-subsystem dispatch — each subsystem gets a
                # handler; order matters (see
                # ``_APPLY_DISPATCH_ORDER``).  Subsystems that
                # require process-level rewiring (MCP processes,
                # integration webhook routing tables, transports,
                # extensions, learning worker subsystem) are handled
                # by ``_apply_restart_required_diff`` which updates
                # the in-memory config and logs a "restart required"
                # warning.  Per-subsystem live restart with rollback
                # is a follow-up to this commit.
                handled = {
                    "name",
                    "mission",
                    "vision",
                    "policies",
                    "roles",
                    "units",
                    "token_budget",
                    "turn_engine",
                    "providers",
                    "notification_rate_limit",
                    "notification_coalesce_window_seconds",
                    "notification_coalesce_max_batch",
                    "learning",
                    "mcp_servers",
                    # Inbound/notification integrations (jira, confluence,
                    # slack, github, forge_app_id, transports) — handled
                    # by the integration branches in _apply_config_diff /
                    # _apply_scalars_diff.
                    "integrations",
                    # Knowledge (confluence_spaces) flows into the org via
                    # config_to_organization and is picked up by the org
                    # diff (see _dispatch_org_diff).
                    "knowledge",
                    "extensions",
                    "scheduling",
                }
                old_dump = old.model_dump(exclude=handled)
                new_dump = new.model_dump(exclude=handled)
                if old_dump != new_dump:
                    # Should never fire — every CompanyConfig field
                    # is in ``handled``.  Belt-and-suspenders against
                    # schema changes that forget to add new fields.
                    # ``old_dump`` and ``new_dump`` exclude the same
                    # keys, so a *symmetric* set diff is always empty;
                    # report the keys whose VALUES diverged instead.
                    diverged = {
                        k for k in old_dump if old_dump.get(k) != new_dump.get(k)
                    }
                    raise NotImplementedError(
                        "apply_config saw an unrecognised Tier B "
                        f"field diff: {diverged or '?'}"
                    )

                logger.info(
                    "apply_config_diff",
                    org=new.name,
                    old_org=old.name,
                )
                # Capture an in-memory snapshot of every field the
                # diff handlers mutate so a mid-apply failure can
                # roll back to the prior state.  Snapshot stores
                # handles + value copies; not deep copies of MCP
                # processes / transport instances (which are recovered
                # by the inverse build_new → swap → dispose pattern
                # in each subsystem handler).
                snapshot = self._snapshot_for_rollback()
                applied: list[str] = []
                failed_subsystem: str | None = None
                entered_restart_required = False
                try:
                    new_org_obj = config_to_organization(new)
                    failed_subsystem = "org"
                    if self._dispatch_org_diff(old, new, new_org_obj):
                        await self._apply_org_diff(self.org, new_org_obj)
                        applied.append("org")
                    failed_subsystem = "budgets"
                    if self._apply_budgets_diff(old, new):
                        applied.append("budgets")
                    failed_subsystem = "turn_engine"
                    if self._apply_turn_engine_diff(old, new):
                        applied.append("turn_engine")
                    failed_subsystem = "providers"
                    if self._apply_providers_diff(old, new):
                        applied.append("providers")
                        # First provider after a zero-provider activation
                        # means the turn engine could not be built at
                        # ``start()``; build it now and subscribe agent
                        # inboxes so routed notifications get consumed.
                        if self._tier_b_done:
                            await self._ensure_turn_engine_after_providers()
                    failed_subsystem = "scalars"
                    if self._apply_scalars_diff(old, new):
                        applied.append("scalars")
                    failed_subsystem = "restart_required"
                    # Past this point rollback cannot fully undo what is
                    # about to change: it restores and restarts
                    # transports, but the per-role MCP children this
                    # block respawns stay on the failed revision.
                    entered_restart_required = True
                    applied.extend(await self._apply_restart_required_diff(old, new))
                    failed_subsystem = None
                except Exception as exc:
                    logger.exception(
                        "config_apply_failed",
                        revision=new.name,
                        subsystem=failed_subsystem or "unknown",
                        error=str(exc),
                    )
                    await self._rollback(snapshot)
                    raise ConfigApplyError(
                        failed_subsystem or "unknown",
                        exc,
                        applied,
                        degraded=entered_restart_required,
                    ) from exc
                # Refresh derived state that the org renders into.
                self.delegation = DelegationHandler(
                    self.org, self.agent_pool, self.event_queue
                )
                self._active_config = new
                self._active_resolution = fingerprint
                logger.info(
                    "config_applied",
                    org=new.name,
                    first_activation=False,
                    applied_subsystems=applied,
                )
                return applied

            logger.info("apply_config_first_activation", org=new.name)

            # Populate Tier-B-driven engine state.
            self.org = config_to_organization(new)
            self.delegation = DelegationHandler(
                self.org, self.agent_pool, self.event_queue
            )
            self._llm_providers = build_llm_providers(new)
            # The sandboxed Execute backend: the manager
            # is ``None`` unless ``providers.sandbox`` is configured, and
            # the verbatim (``${ENV}``-unresolved) provider configs let
            # the backend derive the coding agent's creds per role.
            self._sandbox_manager = build_sandbox_manager(new)
            self._llm_provider_configs = dict(new.providers.llm)
            # Durable detached-run store shared by the kick-off (turn
            # engine) and the completion handler (coordinator).  Postgres
            # for across-restart recovery; memory otherwise.
            if self._sandbox_manager is not None:
                self._sandbox_pending_store = self._build_sandbox_pending_store()
                self._sandbox_otel_receiver = self._build_sandbox_otel_receiver()
            # Build the embeddings provider here so the spawn cascade
            # (``start`` / ``_spawn_company_from_active_config``) wires
            # the learning subsystem — agent_diary, episode store,
            # reflect engine — against the configured provider.  An
            # engine that boots unconfigured has ``self._embeddings is
            # None`` until this first activation; without this the
            # cascade would log ``learning_disabled: no_db_or_embeddings``
            # and the founder's embeddings config would be ignored.
            self._embeddings = build_embedding_provider(new)
            self._pending_extensions = build_extensions(new)
            self._mcp_configs = parse_mcp_servers(new.mcp_servers)
            self._pending_transports = (
                build_notification_transports(new, storage=self.storage)
                + self._custom_transports
            )
            self._github_config = build_github_integration(new, self.org)
            self._gitlab_config = build_gitlab_integration(new, self.org)
            self._plane_config = build_plane_integration(new, self.org)
            self._notification_rate_limit = new.notification_rate_limit
            self._inbox_batch_options.linger_seconds = (
                new.notification_coalesce_window_seconds
            )
            self._inbox_batch_options.max_batch = new.notification_coalesce_max_batch
            # Update the cap in place rather than rebuilding: a fresh
            # BudgetManager would drop the shared usage store wired at
            # boot, silently returning the fleet to per-process counters.
            # It also keeps the meter — and therefore the reporter's hook
            # — alive across an activation, so nothing has to re-attach.
            # Per-agent caps are re-seeded by the org diff below.
            self.budget_manager.update_org_budget(new.token_budget)
            # Per-seat caps are a projection of the active revision, so
            # they land the moment it activates — not when the spawn
            # cascade happens to run.  Idempotent with the reseed in
            # ``start`` step 4 and the one after every org swap.
            self._reseed_seat_budgets(self.org)
            self._turn_engine_config = new.turn_engine
            self._learning_config = new.learning
            self._scheduling_config = new.scheduling
            self._forge_app_id = resolve_forge_app_id(new)
            # Operator-defined skill variables (e.g. tenant base URLs) that
            # tool-skill text references as ${var}.  Stored on the registry
            # so every render site (catalogue summary, loaded body, guard
            # message) substitutes them — see PromptSkillRegistry.render.
            self._skill_variables = resolve_skill_variables(new)
            self._prompt_skill_registry.set_variables(self._skill_variables)
            self._active_config = new
            self._active_resolution = fingerprint

            # If the engine is already running (i.e. the first
            # PUT /config arrived while we were idle in the
            # unconfigured state), kick off the spawn cascade now by
            # re-calling start().  start() is re-entrant: Tier A
            # steps are guarded by ``_tier_a_done`` and skipped on
            # re-entry, so only the Tier B cascade runs.
            if self._running:
                logger.info("spawning_company_post_first_activation", org=new.name)
                await self.start()
            logger.info(
                "config_applied",
                org=new.name,
                first_activation=True,
            )
            return [
                "org",
                "budgets",
                "turn_engine",
                "learning",
                "scheduling",
                "providers",
                "scalars",
                "mcp_servers",
                "notification_transports",
                "integrations",
                "extensions",
            ]

    def _dispatch_org_diff(self, old: Any, new: Any, new_org: Any) -> bool:
        """Return True if the org section actually differs.

        Compares scalar identity fields directly, but for ``roles`` /
        ``units`` (lists of dicts) compares the materialised
        :class:`Organization` outputs.  A founder reordering roles in
        YAML must not trigger a pointless spawn-cascade pass.
        """
        for field in ("name", "mission", "vision", "policies"):
            if getattr(old, field) != getattr(new, field):
                return True

        # Org-wide knowledge spaces (``knowledge.confluence_spaces``)
        # materialise onto ``Organization.confluence_spaces``; a change
        # there must swap ``self.org`` so the new search scope takes
        # effect, even when no role/unit changed.
        if self.org.confluence_spaces != new_org.confluence_spaces:
            return True

        # Same for the org-wide Plane read scope
        # (``knowledge.plane_projects`` → ``Organization.plane_projects``).
        if self.org.plane_projects != new_org.plane_projects:
            return True

        # Order-insensitive comparison for roles + units.  The signature
        # must cover EVERY field ``_apply_org_diff`` reacts to, otherwise
        # a single-field live edit (e.g. a role's ``mcp_env`` — which now
        # also carries the per-agent GitHub/Atlassian identity — ``slack``,
        # ``llm`` / ``llm_*``, ``token_budget``, ``email``, ``unit``,
        # ``learning_enabled``) is silently dropped because
        # this gate returns False and the org diff never runs.
        # ``repr(model_dump())`` captures the full role/unit state
        # regardless of how the models grow; sorting makes a YAML reorder
        # a no-op while any real add/remove/change still fires.
        def _role_signatures(org: Any) -> list[str]:
            return sorted(repr(r.model_dump()) for r in org.all_roles())

        def _unit_signatures(org: Any) -> list[str]:
            # Unit-only fields (purpose, goals, slack_channel, lead,
            # type, knowledge_refs, and the per-unit integration
            # identities jira_project / confluence_space / plane_project
            # that feed the transports' lead maps) feed prompt rendering
            # + routing but are not Role fields, so they wouldn't show
            # up in the role signatures above.  mcp_env / lead effects
            # DO reach roles (inheritance + auto-manage) and are covered
            # there.
            return sorted(
                repr(
                    {
                        "name": u.name,
                        "type": str(u.type),
                        "purpose": u.purpose,
                        "goals": list(u.goals),
                        "lead": u.lead,
                        "slack_channel": u.slack_channel,
                        "knowledge_refs": list(u.knowledge_refs),
                        "jira_project": u.jira_project,
                        "confluence_space": u.confluence_space,
                        "plane_project": u.plane_project,
                    }
                )
                for u in org.all_units()
            )

        # ``self.org`` IS the materialised prior org until
        # ``_apply_org_diff`` swaps it (which runs only AFTER this
        # dispatch returns True).  Use it directly instead of paying
        # for another ``config_to_organization`` pass on every
        # ``revision_activated`` -- the deepcopy + Pydantic validator
        # chain is the most expensive line in the diff hot path.
        return _role_signatures(self.org) != _role_signatures(
            new_org
        ) or _unit_signatures(self.org) != _unit_signatures(new_org)

    def _snapshot_for_rollback(self) -> dict[str, Any]:
        """Capture in-memory state needed to rewind a failed apply.

        Captures both the "pending" config lists AND the live running
        state the per-subsystem handlers mutate in place:
        ``NotificationService.transports`` dict, ``ExtensionManager``
        registered list, MCP bridge client set, and per-role MCP tool
        cache.  Without these, a mid-apply failure leaves the engine
        with extensions unregistered and a transports dict swapped to
        the new shape that rollback can't reach.
        """
        live_transports = (
            dict(self.notification_service.transports)
            if self.notification_service is not None
            else {}
        )
        live_extensions = list(self._extension_manager.extensions)
        return {
            "active_config": self._active_config,
            "org": self.org,
            "delegation": self.delegation,
            "llm_providers": dict(self._llm_providers),
            "mcp_configs": list(self._mcp_configs),
            "pending_transports": list(self._pending_transports),
            "github_config": self._github_config,
            "gitlab_config": self._gitlab_config,
            "plane_config": self._plane_config,
            "notification_rate_limit": self._notification_rate_limit,
            "notification_coalesce_window_seconds": (
                self._inbox_batch_options.linger_seconds
            ),
            "notification_coalesce_max_batch": self._inbox_batch_options.max_batch,
            "budget_org_max": self.budget_manager.org_budget.max_tokens,
            "budget_org_used": self.budget_manager.org_budget.used_tokens,
            "turn_engine_config": self._turn_engine_config,
            "turn_engine_settings_snapshot": (
                self.turn_engine._settings.get()
                if self.turn_engine is not None
                else None
            ),
            "learning_config": self._learning_config,
            "scheduling_config": self._scheduling_config,
            "forge_app_id": self._forge_app_id,
            "skill_variables": dict(self._skill_variables),
            "embeddings": self._embeddings,
            "pending_extensions": list(self._pending_extensions),
            # Live runtime state — captured so rollback restores the
            # ACTUAL services, not just the pending configs.
            "live_transports": live_transports,
            "live_extensions": live_extensions,
            "role_mcp_tools": {
                name: list(tools) for name, tools in self._role_mcp_tools.items()
            },
        }

    async def _rollback(self, snapshot: dict[str, Any]) -> None:
        """Restore engine state from a snapshot.

        Best-effort: an exception inside rollback is logged loudly
        (``config_rollback_failed``) but doesn't re-raise — the
        dashboard banner surfaces the divergence between
        the DB-active row and the engine-applied state.

        **Async on purpose.** It used to be synchronous, which meant the
        transport restore was a dict assignment — reinstalling transport
        objects the failed apply had already ``stop()``ed. The node came
        out of rollback reporting the prior epoch with a dead inbound
        path: every webhook rejected with ``handle_event_after_stop``,
        silently, until someone restarted the process. Restoring
        transports has to *restart* them, and restarting is async.

        What rollback still cannot undo is per-role MCP respawn: the
        children of the failed revision are already running, and
        re-running the spawn sequence for every role inside an
        already-failing apply trades one failure for a longer, less
        predictable one. That is what ``ConfigApplyError.degraded``
        records, and why such a node fails readiness.
        """
        logger.warning(
            "config_rollback_started",
            target=snapshot["active_config"].name
            if snapshot["active_config"] is not None
            else "<unconfigured>",
        )
        try:
            self._active_config = snapshot["active_config"]
            self.org = snapshot["org"]
            self.delegation = snapshot["delegation"]
            # In-place restore of the provider dict — preserves the
            # dict identity TurnEngine captured at construction.
            self._llm_providers.clear()
            self._llm_providers.update(snapshot["llm_providers"])
            self._mcp_configs = snapshot["mcp_configs"]
            self._pending_transports = snapshot["pending_transports"]
            self._github_config = snapshot["github_config"]
            self._gitlab_config = snapshot["gitlab_config"]
            # The PlaneTransport in the restored ``live_transports`` dict
            # carries its own resolved config, so restoring the engine
            # field is all the Plane rollback needs.
            self._plane_config = snapshot["plane_config"]
            self._notification_rate_limit = snapshot["notification_rate_limit"]
            self._inbox_batch_options.linger_seconds = snapshot[
                "notification_coalesce_window_seconds"
            ]
            self._inbox_batch_options.max_batch = snapshot[
                "notification_coalesce_max_batch"
            ]
            # Budget caps: restore the org max_tokens and re-derive the
            # per-seat caps from the restored org.  Used-tokens are left
            # alone throughout (they may have advanced via concurrent
            # agent activity, and the shared store is the truth anyway).
            self.budget_manager.update_org_budget(snapshot["budget_org_max"])
            self._reseed_seat_budgets(self.org)
            self._turn_engine_config = snapshot["turn_engine_config"]
            if (
                self.turn_engine is not None
                and snapshot["turn_engine_settings_snapshot"] is not None
            ):
                self.turn_engine._settings.set(
                    snapshot["turn_engine_settings_snapshot"]
                )
            self._learning_config = snapshot["learning_config"]
            self._scheduling_config = snapshot["scheduling_config"]
            self._forge_app_id = snapshot["forge_app_id"]
            self._skill_variables = snapshot["skill_variables"]
            # Keep the registry's copy in lock-step with the rolled-back
            # engine field — apply_config pairs these two writes, so the
            # rollback must too, or skill text would render with the
            # failed revision's variables.
            self._prompt_skill_registry.set_variables(self._skill_variables)
            self._embeddings = snapshot["embeddings"]
            self._pending_extensions = snapshot["pending_extensions"]

            # Restore live runtime state mutated by the per-subsystem
            # live handlers.  Without these, rollback would leave the
            # running services in their post-apply state even though
            # the stored configs reverted.
            if self.notification_service is not None:
                # Route the restore through the SAME machinery the apply
                # used, rather than assigning the dict back. That is what
                # stops the failed revision's transports, RESTARTS the
                # snapshot's, and re-seeds routing from the now-restored
                # org — a plain assignment reinstalled objects that had
                # already been stopped, leaving the node silently deaf
                # while it reported a healthy epoch. Transports whose
                # identity never changed are skipped on both sides, so
                # nothing is double-started.
                await self._apply_notification_transports_live(
                    list(snapshot["live_transports"].values())
                )
                # Same live-state restore for the GitLab routing config —
                # the integrations diff pushed the failed revision's
                # config onto the running service.
                self.notification_service.set_gitlab_config(self._gitlab_config)
            # Restore ExtensionManager._extensions to the pre-apply
            # set.  Cannot re-run on_engine_start on extensions that
            # were unregistered (their on_engine_stop already fired);
            # the list-level restore at least makes ``extensions``
            # property reads correct and any future stop_all sees the
            # right targets.
            self._extension_manager._extensions = list(snapshot["live_extensions"])
            # Restore the per-role MCP tool cache.  Note: the running
            # MCP processes themselves are not re-spawned by rollback
            # (would require re-running the spawn sequence per role);
            # the cache restore at least keeps the engine's view of
            # which tools belong to which role consistent with the
            # pre-apply state.
            self._role_mcp_tools.clear()
            self._role_mcp_tools.update(snapshot["role_mcp_tools"])

            logger.info("config_rollback_succeeded")
        except Exception as exc:
            logger.exception(
                "config_rollback_failed",
                error=str(exc),
                hint=(
                    "Engine may be in an inconsistent state.  Dashboard "
                    "banner shows divergence; restart to recover cleanly."
                ),
            )

    async def _subscribe_activation_nudge(self) -> None:
        """Wake the reconcile loop the moment a revision activates.

        A **nudge only** — it carries no payload the loop trusts and
        skipping it costs at most one poll interval.  That distinction is
        the whole point of the rewrite: this used to be a *durable
        competing-consumer* subscription (group ``engine-config``) that
        did the apply itself, so with N engine processes exactly ONE
        applied any given revision and the rest ran the previous company
        indefinitely.  Deleted roles kept answering Slack, rotated
        credentials kept being used, and the dashboard reported success
        because the one node that applied it published ``ok``.

        Broadcasting it (``subscribe_stream``) fixes the fan-out but not
        the reliability: an ephemeral stream consumer starts at the
        latest message, so anything published while a node reconnects is
        simply gone, and there is no cursor to replay from.  Hence the
        split — the poll in :meth:`_reconcile_config_loop` is
        authoritative because it *asks*, and this only makes the common
        case fast.
        """
        from crewlet.events.types import ConfigRevisionActivated

        store = getattr(self, "_company_config_store", None)
        if store is None:
            logger.debug(
                "skip_activation_nudge_subscription",
                reason="no company_config_store on engine",
            )
            return

        async def _handle(topic: str, event: ConfigRevisionActivated) -> None:
            logger.info(
                "config_activation_nudge",
                revision_id=getattr(event, "revision_id", ""),
                source=getattr(event, "source", ""),
            )
            self._reconcile_nudge.set()

        self._nudge_unsubscribe = await self.event_queue.subscribe_stream(
            "crewlet.config.revision_activated",
            _handle,
        )
        logger.info("subscribed_to_activation_nudge")

    # ── control plane ────────────────────────────────────────────────

    def _config_plane(self) -> Any:
        """The activation log + apply-status store.

        Falls back to the in-memory twin without a database — the same
        rule the lease, budget and rate-limit stores follow.  It is the
        *correct* plane for that shape rather than a stub: with no shared
        database there is also no shared config store, so there is
        exactly one process and nothing to converge with.  Keeping the
        seam a real object (instead of ``None``) is what lets the
        reconcile path have one implementation instead of two.
        """
        from crewlet.db.client import Database

        if self._config_plane_store is not None:
            return self._config_plane_store
        if not isinstance(self.storage, Database):
            from crewlet.db.config_plane import MemoryConfigPlaneStore

            self._config_plane_store = MemoryConfigPlaneStore()
            return self._config_plane_store
        if self._config_plane_store is None and isinstance(self.storage, Database):
            from crewlet.db.config_plane import ConfigPlaneStore

            self._config_plane_store = ConfigPlaneStore(self.storage)
        return self._config_plane_store

    @property
    def posture(self) -> Any:
        """This node's current config posture. See :mod:`crewlet.db.config_plane`."""
        from crewlet.db.config_plane import Posture

        return self._posture or Posture.SERVE

    def admits_triggers(self) -> bool:
        """Whether this node should accept NEW work right now.

        The gate sits on trigger admission rather than on ``run_turn``,
        for two reasons the review gate made concrete. A stale node still
        *consumes* inbound messages — and Slack HMAC verification happens
        consume-side against this node's cached secret, where a failure
        is a skip plus an ack, so a rotated signing secret means it
        silently eats every message it wins. And refusing at ``run_turn``
        would strand a seat whose sandbox run just completed: the pending
        row is already flipped to ``resumed``, the box collected, the
        inbox paused, and nothing reaps a ``resumed`` row in-process.
        """
        from crewlet.db.config_plane import Posture

        return self.posture not in (Posture.SHED, Posture.STUCK)

    async def _apply_posture(self, posture: Any) -> None:
        """Pause or resume trigger topics to match ``posture``.

        Uses the ``config`` pause reason, so releasing it cannot un-gate a
        seat the sandbox is holding, and the sandbox releasing its own
        hold cannot un-gate a diverged node.
        """
        from crewlet.db.config_plane import Posture

        if posture == self._posture:
            return
        previous, self._posture = self._posture, posture
        shed = posture in (Posture.SHED, Posture.STUCK)
        was_shed = previous in (Posture.SHED, Posture.STUCK)
        if shed == was_shed:
            return

        # Seat inboxes are handled by RELEASING the seats below — a
        # diverged node must stop holding them, not merely stop reading
        # them, or it reserves fleet capacity it refuses to use. What is
        # left here is the ingress topic, which belongs to no seat.
        pairs = [("crewlet.notifications.inbound", "notifications")]
        for topic, group in pairs:
            try:
                if shed:
                    await self.event_queue.pause_topic(topic, group, reason="config")
                else:
                    await self.event_queue.resume_topic(topic, group, reason="config")
            except Exception:
                logger.exception(
                    "config_posture_topic_failed", topic=topic, group=group
                )
        # Ownership follows posture. A node that cannot apply the
        # current epoch must not merely stop serving its seats — it must
        # stop HOLDING them, or it reserves fleet capacity for itself
        # while refusing to use it, and its seats stay dark until it
        # converges. Fenced, not voluntary: it is not draining
        # gracefully, it is diverged, and a peer should have the seat
        # now rather than when this node's current turn happens to end.
        if self._seat_host is not None:
            from crewlet.seat.host import ReleaseReason

            try:
                if shed:
                    await self._seat_host.begin_drain()
                    await self._seat_host.release_all(ReleaseReason.POSTURE)
                elif self._running and not self._shutting_down:
                    await self._seat_host.resume_claiming()
            except Exception:
                logger.exception("config_posture_seat_handoff_failed")
        logger.warning(
            "config_posture_changed",
            posture=str(posture),
            previous=str(previous or "serve"),
            shedding=shed,
        )

    async def _reconcile_config_once(self) -> Any:
        """One reconcile tick: converge if behind, then set posture."""
        from crewlet.db.config_plane import (
            MAX_APPLY_ATTEMPTS,
            FleetView,
            Posture,
            decide_posture,
        )

        plane = self._config_plane()
        store = getattr(self, "_company_config_store", None)
        if plane is None or store is None:
            return Posture.SERVE

        # The embedded API's cached projection (webhook secrets above
        # all) follows the same pointer.  Driven here rather than from
        # its own loop so one process polls once — see
        # crewlet.api.config_refresh.
        if self._api_refresher is not None:
            try:
                await self._api_refresher.refresh_if_changed()
            except Exception:
                logger.exception("embedded_api_state_refresh_failed")

        target = await plane.target()
        if target is None:
            return Posture.SERVE

        # A new revision gets a fresh budget. ``_apply_attempts`` counts
        # attempts at ONE epoch; carrying it across an activation makes
        # STUCK terminal for the life of the process, so the operator
        # does exactly what the runbook says — push a fixed revision —
        # and the node never tries it, never says why, and stays shed.
        if target.epoch != self._apply_attempts_epoch:
            if self._apply_attempts >= MAX_APPLY_ATTEMPTS:
                logger.info(
                    "config_apply_attempts_reset",
                    previous_epoch=self._apply_attempts_epoch,
                    epoch=target.epoch,
                    hint=(
                        "a new revision was activated after this node "
                        "exhausted its attempts on the previous one; "
                        "trying again against the new target"
                    ),
                )
            self._apply_attempts = 0
            self._apply_attempts_epoch = target.epoch

        if (
            self._applied_epoch < target.epoch
            and self._apply_attempts < MAX_APPLY_ATTEMPTS
        ):
            self._apply_attempts += 1
            await self._converge_to(target, plane, store)

        self._ticks_behind = (
            self._ticks_behind + 1 if self._applied_epoch < target.epoch else 0
        )
        # Re-stamp this node's status EVERY tick, not only when it
        # converges. ``peer_health`` reads these rows to answer "is there
        # a healthy peer right now", and bounds them on freshness because
        # ``record_apply`` upserts on node_id and a terminated node's
        # ``ok`` would otherwise stand forever. That bound is only honest
        # if a live node keeps writing: a converged node used to record
        # once and go quiet, so its perfectly good status aged out and a
        # lagging peer read ``peers_ok=0`` off a healthy fleet — WAIT or
        # ISOLATED where the truth is SHED. One idempotent upsert per
        # node per tick is what makes the row mean "alive, at this
        # epoch" instead of "was alive, once".
        await self._heartbeat_apply_status(plane)
        peers_ok, peers_reported = await plane.peer_health(
            target.epoch, exclude_node=self._node_id
        )
        posture = decide_posture(
            FleetView(
                target_epoch=target.epoch,
                applied_epoch=self._applied_epoch,
                ticks_behind=self._ticks_behind,
                attempts=self._apply_attempts,
                peers_ok=peers_ok,
                peers_reported=peers_reported,
                self_status=self._apply_status,
            )
        )
        if posture == Posture.ISOLATED:
            logger.error(
                "config_revision_unapplied_fleet_wide",
                epoch=target.epoch,
                hint=(
                    "no node applied this revision — it is probably the "
                    "revision, not this node. Serving the prior config."
                ),
            )
        await self._apply_posture(posture)
        if posture not in (Posture.SHED, Posture.STUCK):
            await self._ensure_ingress_consuming()
        return posture

    async def _ensure_ingress_consuming(self) -> None:
        """Un-quiesce the ingress consumer if a shed left it stopped.

        Reconciled on every admitting tick rather than fired on the
        recovery edge, because the two things involved run on different
        paths: the notification service refuses (and so quiesces) from
        the DELIVERY path, while the posture changes on this loop. An
        edge can therefore fire just before the shed's last in-flight
        delivery quiesces a consumer that nothing would then restart —
        and an ingress consumer that never restarts is a node that
        accepts webhooks and reads none of them, on a company that has
        otherwise fully recovered.

        Converging on "if I admit work, I am consuming" cannot lose that
        race: the next tick fixes it. Idempotent and cheap — the call is
        a no-op when nothing is quiesced, and un-quiescing a topic that
        is also PAUSED changes nothing, since the pause gates delivery
        independently.
        """
        try:
            await self.event_queue.unquiesce(
                "crewlet.notifications.inbound", "notifications"
            )
        except Exception:
            logger.exception("ingress_unquiesce_failed")

    async def _heartbeat_apply_status(self, plane: Any) -> None:
        """Re-assert this node's last apply outcome, unchanged.

        Never fatal: a plane that cannot take the write leaves the
        previous row, which ages out and makes this node look absent —
        the same conservative direction the freshness bound already
        takes. Failing the tick instead would stop the reconcile loop
        that is the only way out of a divergence.
        """
        if self._apply_status is None or not self._applied_epoch:
            return
        try:
            await plane.record_apply(
                self._node_id,
                epoch=self._applied_epoch,
                revision_id=self._applied_revision_id,
                status=self._apply_status,
                error=self._apply_error,
            )
        except Exception:
            logger.debug("config_status_heartbeat_failed", node=self._node_id)

    async def _converge_to(self, target: Any, plane: Any, store: Any) -> None:
        """Apply ``target`` and record the outcome for the fleet."""
        from crewlet.config import CompanyConfig
        from crewlet.db.config_plane import ApplyStatus
        from crewlet.events.types import ConfigRevisionApplied
        from crewlet.secrets import load_config

        status = ApplyStatus.OK
        error = ""
        applied_subsystems: list[str] = []
        try:
            revision = await store.get_revision(target.revision_id)
            if revision is None:
                raise RuntimeError(f"revision {target.revision_id} not in store")
            # Decrypt the encrypted-document payload before validation.
            new = CompanyConfig.model_validate(
                load_config(revision.payload, self._cipher)
            )
            applied_subsystems = await self.apply_config(new) or []
        except ConfigApplyError as exc:
            # Rollback has already run.  ``degraded`` distinguishes the
            # case rollback could not undo (MCP children not respawnable,
            # transports reinstalled already-stopped) — the control plane
            # must never count such a node as somewhere work can go.
            status = ApplyStatus.DEGRADED if exc.degraded else ApplyStatus.ERROR
            error = f"{exc.subsystem}: {exc.original}"
            applied_subsystems = list(exc.applied_before_failure)
            logger.error(
                "config_converge_failed",
                epoch=target.epoch,
                revision_id=str(target.revision_id),
                subsystem=exc.subsystem,
                degraded=exc.degraded,
                error=str(exc.original),
            )
        except Exception as exc:
            status = ApplyStatus.ERROR
            error = str(exc)
            logger.exception(
                "config_converge_failed",
                epoch=target.epoch,
                revision_id=str(target.revision_id),
            )
        else:
            self._applied_epoch = target.epoch
            self._apply_attempts = 0
            self._apply_attempts_epoch = target.epoch
            self._ticks_behind = 0
            logger.info(
                "config_converged",
                epoch=target.epoch,
                revision_id=str(target.revision_id),
                subsystems=applied_subsystems,
            )

        self._apply_status = status
        self._applied_revision_id = target.revision_id
        self._apply_error = error
        try:
            await plane.record_apply(
                self._node_id,
                epoch=target.epoch,
                revision_id=target.revision_id,
                status=status,
                error=error,
            )
        except Exception:
            # The write is how peers learn this node's outcome; losing it
            # makes the node look silent rather than failed, which
            # ``decide_posture`` reads as "no evidence" — the conservative
            # direction.  The local apply already happened either way.
            logger.exception("config_apply_status_write_failed")

        await self.event_queue.publish(
            "crewlet.config.revision_applied",
            ConfigRevisionApplied(
                source="engine.reconcile",
                revision_id=str(target.revision_id),
                status=str(status),
                applied_subsystems=applied_subsystems,
                error=error,
            ),
        )

    async def _reconcile_config_loop(self) -> None:
        """Poll the activation pointer forever.

        The AUTHORITATIVE delivery mechanism, deliberately — see
        :meth:`_subscribe_activation_nudge` for why the event it replaced
        could not be.  A poll cannot miss anything, because it asks.

        The nudge shortens the wait but never replaces the tick: after a
        nudge fires, the next iteration still sleeps a full jittered
        interval, so an activation storm cannot turn into an apply storm.
        """
        from crewlet.db.config_plane import reconcile_delay

        # Runs from Tier-A boot until :meth:`_stop_control_plane`
        # cancels it — deliberately NOT gated on ``self._running``, which
        # only flips true at the END of the Tier-B cascade.  An
        # unconfigured engine has to reconcile: the first activation is
        # precisely what brings it to life.
        while True:
            try:
                with contextlib.suppress(TimeoutError):
                    await asyncio.wait_for(
                        self._reconcile_nudge.wait(), timeout=reconcile_delay()
                    )
                self._reconcile_nudge.clear()
                await self._reconcile_config_once()
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.exception("config_reconcile_tick_failed")

    async def _stop_control_plane(self) -> None:
        """Cancel the reconcile loop and drop the activation nudge.

        Called before the rest of the teardown: the reconciler can start
        a full apply — respawning MCP children, rebuilding transports —
        and one that lands mid-shutdown would resurrect subsystems the
        drain is trying to bring down.
        """
        if self._reconcile_task is not None:
            task, self._reconcile_task = self._reconcile_task, None
            await cancel_and_wait(task)
        if self._nudge_unsubscribe is not None:
            unsubscribe, self._nudge_unsubscribe = self._nudge_unsubscribe, None
            with contextlib.suppress(Exception):
                await unsubscribe()
        if self._api_refresher is not None:
            refresher, self._api_refresher = self._api_refresher, None
            with contextlib.suppress(Exception):
                await refresher.stop()

    async def _seed_applied_epoch(self, revision_id: Any) -> None:
        """Record the epoch the boot-time apply of ``revision_id`` satisfied.

        Boot applies the *active revision*; the reconcile loop converges
        on the *activation pointer*.  Those agree in the ordinary case,
        and when they do this node is already at that epoch — recording
        it is what stops the first tick from re-applying (and restarting
        every MCP child) seconds after boot.

        When they disagree — an activation landed between the boot read
        and this call — the epoch is deliberately left unseeded so the
        first tick converges properly.
        """
        from crewlet.db.config_plane import ApplyStatus

        plane = self._config_plane()
        if plane is None:
            return
        try:
            target = await plane.target()
            if target is None or str(target.revision_id) != str(revision_id):
                return
            self._applied_epoch = target.epoch
            self._apply_status = ApplyStatus.OK
            self._applied_revision_id = target.revision_id
            self._apply_error = ""
            await plane.record_apply(
                self._node_id,
                epoch=target.epoch,
                revision_id=target.revision_id,
                status=ApplyStatus.OK,
            )
            logger.info("config_epoch_seeded", epoch=target.epoch)
        except Exception:
            logger.exception("config_epoch_seed_failed")

    async def start(self) -> None:
        """Boot the engine.

        Re-entrant: when called on a running unconfigured engine, runs
        only the Tier-B spawn cascade.  ``apply_config`` re-calls
        ``start()`` on first-activation to bring an unconfigured-boot
        engine fully alive without restart.

        1. Initialize storage backend
        2. Start event queue
        3. Start observability
        4. Spawn agents from org
        5. Launch MCP servers
        6. Set up notifications
        7. Set up turn engine
        8. Register extensions and subscriptions
        """
        # Stamped before either branch below: ``start()`` is re-entrant
        # (an unconfigured boot runs only Tier A, and activation re-calls
        # it for the Tier B cascade), and the engine has been up since
        # the first of those, not the second.
        if not self._started_at:
            self._started_at = datetime.now(UTC).isoformat()
        if not getattr(self, "_tier_a_done", False):
            logger.info("engine_starting", org=self.org.name)

            # 0. Initialize OpenTelemetry tracing

            from crewlet.telemetry import init_telemetry

            init_telemetry(
                otlp_endpoint=os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
                service_name=f"crewlet.{self.org.name}",
            )

            # 1. Initialize storage if it has an initialize method
            logger.info("start_step", step="1/8", action="init_storage")
            if hasattr(self.storage, "initialize"):
                await self.storage.initialize()

            # 1.5 Point the budget cascade at the shared counter. Token
            # budgets are a company-wide question ("have we spent our
            # 500k"), so the in-memory default is only correct for a
            # single process — with peers, an org cap silently becomes
            # N x the configured value.
            from crewlet.db.budgets import PostgresBudgetUsageStore
            from crewlet.db.client import Database

            if isinstance(self.storage, Database):
                self.budget_manager.set_usage_store(
                    PostgresBudgetUsageStore(self.storage)
                )

            # 2. Start event queue
            logger.info("start_step", step="2/8", action="start_queues")
            await self.event_queue.start()
            self._queue_started = True
            await self._apply_pending_hooks()

            # 3. Start observability manager
            logger.info("start_step", step="3/8", action="start_observability")
            await self.observability.start()

            # 3.5. Attach the control plane so the node picks up live
            # config edits.  Both halves run regardless of whether a
            # Tier B config is currently active — an unconfigured engine
            # wakes up on the first activation.
            #
            # The reconcile LOOP is authoritative; the broadcast nudge
            # only makes the common case fast.  See
            # ``_subscribe_activation_nudge``.
            self._node_id = resolve_node_id(getattr(self, "_bootstrap", None))
            self._resolve_node_profile()
            await self._subscribe_activation_nudge()
            if self._reconcile_task is None:
                self._reconcile_task = asyncio.create_task(
                    self._reconcile_config_loop()
                )

            self._tier_a_done = True

        # Tier-B gate: when no active CompanyConfig, stay in the
        # unconfigured state.  The API socket and Pulsar subscriptions
        # are already up (Tier A); the operator pushes the first
        # revision via PUT /config or `crewlet config import`.
        # ``apply_config`` will re-call ``start()`` once the first
        # revision arrives — the cascade below then runs.
        if self.org.name == "" and self._active_config is None:
            if not self._running:
                logger.info("engine_started_unconfigured")
                self._running = True
            return

        # Idempotent: if the spawn cascade already ran, do nothing.
        if self._tier_b_done:
            return

        # 4. Seat placement — NOT "spawn every agent".
        #
        # An agent instance is process-local state that is never
        # persisted: its ``AgentState`` (WORKING, AWAITING_SANDBOX) lives
        # only here. Spawning one on every node therefore does not give
        # the fleet a warm spare, it gives it N copies of a state machine
        # that disagree — a non-owner's instance strands in
        # AWAITING_SANDBOX and the seat comes home poisoned.
        #
        # So the pool holds exactly the seats this node OWNS, and the
        # placement host decides which those are. The actual spawning
        # happens in ``_acquire_seat``, which also attaches the consumer
        # — last, after the seat is established.
        role_count = len(list(self.org.all_roles()))
        logger.info(
            "start_step", step="4/8", action="place_seats", role_count=role_count
        )
        # Set per-seat token budgets from role configs.  Driven by the
        # ORG, not the pool: a cap must exist for every seat this node
        # could ever run, and under owner-only seats that is every seat
        # in the company, not the subset spawned here.
        self._reseed_seat_budgets(self.org)
        if self._seat_host is None:
            self._seat_host = self._build_seat_host()
        await self._ensure_seat_subscriptions()

        # 5. Launch MCP servers (if configured)
        logger.info(
            "start_step",
            step="5/8",
            action="start_mcp_servers",
            count=len(self._mcp_configs),
        )
        await self._start_mcp_servers()

        # 6. Initialize notification service (before the turn engine so
        #    the engine can reference it for agent tools)
        logger.info("start_step", step="6/8", action="init_notifications")
        from crewlet.notifications.handle import (
            HandleRegistry,
            register_human_contacts_from_org,
        )

        transports_dict: dict[str, Any] = {}
        for transport in self._pending_transports:
            transports_dict[transport.name] = transport
        self._share_delivery_dedupe(transports_dict)

        # The org provider keeps human-seat resolution live across
        # hot reloads that swap ``self.org``.
        self.handle_registry = HandleRegistry(
            self.agent_pool, org_provider=lambda: self.org
        )
        # Human seats declare their external IDs in config — register
        # them up front (no MCP resolution involved) so sender
        # attribution and party lookup work from the first webhook.
        register_human_contacts_from_org(self.handle_registry, self.org)
        # Let the A2A service defend its own contract (bus targets
        # must be live agents — human seats / typos get refused at
        # the chokepoint instead of waking a subscriber-less topic).
        if self.a2a_service is not None:
            self.a2a_service.set_handle_registry(self.handle_registry)
        self.notification_service = NotificationService(
            event_queue=self.event_queue,
            transports=transports_dict,
            handle_registry=self.handle_registry,
            rate_limit=self._notification_rate_limit,
        )
        # A stale node must stop consuming inbound before it stops
        # running turns — see NotificationService._handle_inbound.
        self.notification_service.set_admission_gate(self.admits_triggers)

        # Register per-agent Slack apps from role configs
        from crewlet.notifications.transports.slack import (
            SlackTransport,
        )

        slack_transport = transports_dict.get("slack")
        if isinstance(slack_transport, SlackTransport):
            slack_transport.set_handle_registry(self.handle_registry)
            register_slack_apps_from_org(slack_transport, self.org)

        # Register per-agent Mattermost bots from role configs.  Unlike
        # every other transport this one also needs the event queue: it
        # owns the inbound websocket fleet, so it is a PRODUCER of
        # inbound events rather than only a consumer of outbound ones.
        from crewlet.notifications.transports.mattermost import MattermostTransport

        mattermost_transport = transports_dict.get("mattermost")
        if isinstance(mattermost_transport, MattermostTransport):
            mattermost_transport.set_handle_registry(self.handle_registry)
            mattermost_transport.set_event_queue(self.event_queue)
            register_mattermost_bots_from_org(mattermost_transport, self.org)

        from crewlet.notifications.transports.jira import JiraTransport

        jira_transport = transports_dict.get("jira")
        if isinstance(jira_transport, JiraTransport):
            # Wire handle registry for self-ignore checks
            jira_transport.set_handle_registry(self.handle_registry)

            # Build project key → lead handle mapping for fallback routing
            from crewlet.config import build_project_key_lead_map

            project_key_leads = build_project_key_lead_map(self.org)
            if project_key_leads:
                jira_transport.set_project_key_leads(project_key_leads)

            if self.mcp_bridge:
                try:
                    await register_jira_accounts_from_org(
                        self.handle_registry,
                        self.org,
                        mcp_bridge=self.mcp_bridge,
                    )
                except Exception as exc:
                    logger.error("jira_account_registration_failed", error=str(exc))

        # Register GitHub usernames for webhook routing
        if self._github_config and self._github_config.enabled and self.mcp_bridge:
            try:
                await register_github_accounts_from_org(
                    self.handle_registry,
                    self.org,
                    mcp_bridge=self.mcp_bridge,
                )
            except Exception as exc:
                logger.error("github_account_registration_failed", error=str(exc))

        # Register GitLab usernames for webhook routing (REST GET /user;
        # no MCP bridge needed — see register_gitlab_accounts_from_org)
        if self._gitlab_config and self._gitlab_config.enabled:
            try:
                await register_gitlab_accounts_from_org(
                    self.handle_registry,
                    self.org,
                    gitlab_config=self._gitlab_config,
                )
            except Exception as exc:
                logger.error("gitlab_account_registration_failed", error=str(exc))
        # Wire the config into the notification service so _parse_gitlab
        # can do participants-based routing (integrations.gitlab.token).
        if self.notification_service is not None:
            self.notification_service.set_gitlab_config(self._gitlab_config)

        from crewlet.notifications.transports.plane import PlaneTransport

        # One knowledge backend at a time: config validation
        # (confluence × plane exclusivity) guarantees at most one of
        # the two transports exists; the branches below do only the
        # transport-specific ROUTING wiring, while the knowledge
        # machinery (searcher / sync worker / index callback) is built
        # once afterwards by ``_install_knowledge_backend`` — the same
        # shared selection the live-refresh reconcile and the promotion
        # gate use.  Neither enabled ⇒ searcher None + no worker: the
        # Plan prefetch renders nothing, the skills registry stays
        # empty, and the promotion pass is a no-op — all soft.
        self._plane_transport: Any = None

        plane_transport = transports_dict.get("plane")
        # Plane project where tool-skill pages live ("" disables the
        # sync AND the searcher's skills-project exclusion).  Same
        # contract as CREWLET_TOOL_SKILLS_SPACE.
        self._tool_skill_project_key = os.environ.get(
            "CREWLET_TOOL_SKILLS_PROJECT", "TS"
        )
        if isinstance(plane_transport, PlaneTransport):
            self._plane_transport = plane_transport
            # Wire handle registry for mention / self-ignore resolution
            plane_transport.set_handle_registry(self.handle_registry)

            # Build project identifier → lead handle mapping for
            # fallback routing (unassigned work items, intake triage,
            # page events).
            from crewlet.config import build_plane_project_lead_map

            plane_leads = build_plane_project_lead_map(self.org)
            if plane_leads:
                plane_transport.set_project_leads(plane_leads)

            # Tool Skills project webhooks have no human/agent recipient;
            # suppress the notification-routing path for them (the index
            # callback still fires — see set_notification_excluded_projects).
            if self._tool_skill_project_key:
                plane_transport.set_notification_excluded_projects(
                    [self._tool_skill_project_key]
                )

        # Register Plane user UUIDs for webhook routing (REST
        # GET /users/me/ per role token; no MCP bridge needed — see
        # register_plane_accounts_from_org)
        if self._plane_config and self._plane_config.enabled:
            try:
                await register_plane_accounts_from_org(
                    self.handle_registry,
                    self.org,
                    plane_config=self._plane_config,
                )
            except Exception as exc:
                logger.error("plane_account_registration_failed", error=str(exc))

        from crewlet.notifications.transports.confluence import ConfluenceTransport

        confluence_transport = transports_dict.get("confluence")
        # Confluence space key where tool-skill pages live. Overridable
        # via env so operators with house naming conventions don't have
        # to fork the engine. ``""`` disables the tool-skill sync.
        self._tool_skill_space_key = os.environ.get("CREWLET_TOOL_SKILLS_SPACE", "TS")
        self._confluence_transport: Any = (
            confluence_transport
            if isinstance(confluence_transport, ConfluenceTransport)
            else None
        )
        if isinstance(confluence_transport, ConfluenceTransport):
            confluence_transport.set_handle_registry(self.handle_registry)

            # Build space key → lead handle mapping for routing
            from crewlet.config import build_space_key_lead_map

            space_key_leads = build_space_key_lead_map(self.org)
            if space_key_leads:
                confluence_transport.set_space_key_leads(space_key_leads)

            # Tool Skills space webhooks update the in-memory
            # PromptSkillRegistry via the index callback that
            # ``_install_knowledge_backend`` registers below; they have
            # no human/agent recipient, so suppress the
            # notification-routing path for them.  Without this, every
            # tool-skill page edit surfaces as a notification_undeliverable
            # warning + notification_skipped event.
            if self._tool_skill_space_key:
                confluence_transport.set_notification_excluded_spaces(
                    [self._tool_skill_space_key]
                )

        # Knowledge machinery (query-time searcher + Tool Skills sync
        # worker + index callback) for the selected backend — one shared
        # selection + wiring path (`_select_knowledge_backend` /
        # `_install_knowledge_backend`), also used by the live-refresh
        # reconcile, so boot and refresh can never disagree on which
        # backend is active.
        self._install_knowledge_backend()

        # 6.5 Initialize the agent-learning subsystem.
        #     The episode store requires a real Database + EmbeddingProvider.
        #     Reflect + persist requires an LLM provider; the diary-backed
        #     tools additionally need a Database.
        from crewlet.db.client import Database

        self._episode_store = None
        self._reflect_engine = None
        self._episode_lifecycle_worker = None
        self._skill_curator_worker = None
        self._counterparty_store = None
        self._synthesized_skill_store = None
        lrn_cfg = getattr(self, "_learning_config", None)
        learning_enabled = bool(lrn_cfg.enabled) if lrn_cfg is not None else True
        reflect_cfg = getattr(lrn_cfg, "reflect", None) if lrn_cfg is not None else None
        counterparty_cfg = (
            getattr(lrn_cfg, "counterparty", None) if lrn_cfg is not None else None
        )
        skill_synth_cfg = (
            getattr(lrn_cfg, "skill_synthesis", None) if lrn_cfg is not None else None
        )
        skill_refine_cfg = (
            getattr(lrn_cfg, "skill_refinement", None) if lrn_cfg is not None else None
        )
        skill_promo_cfg = (
            getattr(lrn_cfg, "skill_promotion", None) if lrn_cfg is not None else None
        )
        personal_memory_cfg = (
            getattr(lrn_cfg, "personal_memory", None) if lrn_cfg is not None else None
        )
        episode_lifecycle_cfg = (
            getattr(lrn_cfg, "episode_lifecycle", None) if lrn_cfg is not None else None
        )
        summarize_enabled = bool(
            reflect_cfg.summarize_episodes if reflect_cfg is not None else True
        )
        summarize_max_tokens = int(
            reflect_cfg.summarize_max_tokens if reflect_cfg is not None else 400
        )
        if learning_enabled:

            def _role_lookup(handle: str) -> Any:
                # Shared by all learning tools that need the caller's
                # role for aux-provider routing (query_episodes,
                # refresh_memory).  Cheap; the agent_pool lookup is
                # in-memory.
                agent = self.agent_pool.get_by_handle(handle) if handle else None
                if agent is None:
                    return None
                return agent.definition.role

            if isinstance(self.storage, Database) and self._embeddings is not None:
                from crewlet.learning.episode_store import EpisodeStore
                from crewlet.learning.tools import register_episode_tools

                # Wire the lifecycle trigger directly into EpisodeStore
                # so each write checks the per-agent threshold and
                # publishes ``CompactionRequested`` when crossed.  The
                # EpisodeLifecycleWorker (set up further down, after
                # reflect-engine wiring) consumes those events.
                lifecycle_threshold = (
                    int(episode_lifecycle_cfg.max_raw_episodes_per_agent)
                    if episode_lifecycle_cfg is not None
                    else 500
                )
                lifecycle_check_every = (
                    int(episode_lifecycle_cfg.write_check_every_n)
                    if episode_lifecycle_cfg is not None
                    else 10
                )
                self._episode_store = EpisodeStore(
                    self.storage,
                    self._embeddings,
                    event_queue=self.event_queue,
                    max_raw_episodes_per_agent=lifecycle_threshold,
                    write_check_every_n=lifecycle_check_every,
                )

                register_episode_tools(
                    self.tool_registry,
                    self._episode_store,
                    llm_providers=self._llm_providers or None,
                    summarize=summarize_enabled,
                    summarize_max_tokens=summarize_max_tokens,
                    role_lookup=_role_lookup if self._llm_providers else None,
                )
                logger.info("learning_enabled", subsystem="episodes")
            else:
                logger.info(
                    "learning_disabled",
                    reason="no_db_or_embeddings",
                    has_db=isinstance(self.storage, Database),
                    has_embeddings=self._embeddings is not None,
                )

            # Reflect + persist.  Requires at least one LLM
            # provider; the diary-backed tools additionally need a
            # Database.
            reflect_enabled = (
                bool(reflect_cfg.enabled) if reflect_cfg is not None else True
            )
            persist_decider_enabled = (
                bool(reflect_cfg.persist_decider) if reflect_cfg is not None else True
            )
            persist_budget = int(
                reflect_cfg.budget_tokens if reflect_cfg is not None else 5000
            )
            if reflect_enabled:
                from crewlet.learning.diary import AgentDiary
                from crewlet.learning.onboarding import register_mark_onboarded_tool
                from crewlet.learning.reflect_engine import ReflectEngine
                from crewlet.learning.tools import (
                    register_reflect_and_persist_tool,
                    register_refresh_memory_tool,
                )

                # AgentDiary requires a real Database for the
                # ``agent_diary`` table; skip wiring (and the tools
                # that depend on it) in in-memory / test mode.
                self._agent_diary: AgentDiary | None = None
                if isinstance(self.storage, Database):
                    self._agent_diary = AgentDiary(self.storage, self._embeddings)
                    register_reflect_and_persist_tool(
                        self.tool_registry, self._agent_diary
                    )
                    logger.info("learning_enabled", subsystem="agent_diary")
                else:
                    logger.info("agent_diary_disabled", reason="no_database")
                # refresh_memory: lets the planner
                # re-run the personal-memory filter mid-turn after
                # gathering context (e.g. reading a Slack thread when
                # the trigger was just "yes").  Requires LLM providers
                # to drive the filter AND a wired AgentDiary; without
                # either the tool would always fail, so skip
                # registration.
                if self._llm_providers and self._agent_diary is not None:
                    refresh_cap = (
                        int(personal_memory_cfg.max_refreshes_per_turn)
                        if personal_memory_cfg is not None
                        else 3
                    )
                    register_refresh_memory_tool(
                        self.tool_registry,
                        self._agent_diary,
                        llm_providers=self._llm_providers,
                        role_lookup=_role_lookup,
                        max_refreshes_per_turn=refresh_cap,
                    )
                # Agent-driven onboarding marker.  Convention is one
                # Confluence page titled 'Onboarding' per unit; the
                # agent reads them itself (via existing
                # confluence_get_page) and calls ``mark_onboarded``
                # when done.  Markers live in their own table,
                # ``agent_onboarding_markers`` (one row per agent,
                # UPSERT-keyed).
                self._onboarding_marker_store: Any = None
                if isinstance(self.storage, Database):
                    from crewlet.learning.onboarding_markers import (
                        OnboardingMarkerStore,
                    )

                    self._onboarding_marker_store = OnboardingMarkerStore(self.storage)
                    register_mark_onboarded_tool(
                        self.tool_registry, self._onboarding_marker_store, self.org
                    )
                # CounterpartyStore + Profiler.  Require a
                # Database for the profile table.  Profiler also needs
                # at least one LLM provider; when absent we still
                # create the store (so lookup_colleague / Plan auto-inject
                # can read existing profiles) but skip the profiler.
                counterparty_enabled = (
                    bool(counterparty_cfg.enabled)
                    if counterparty_cfg is not None
                    else True
                )
                counterparty_profiler: Any = None
                if counterparty_enabled and isinstance(self.storage, Database):
                    from crewlet.learning.counterparty_profiler import (
                        CounterpartyProfiler,
                    )
                    from crewlet.learning.counterparty_store import CounterpartyStore

                    self._counterparty_store = CounterpartyStore(self.storage)
                    if self._llm_providers:
                        counterparty_profiler = CounterpartyProfiler(
                            llm_providers=self._llm_providers,
                            store=self._counterparty_store,
                            budget_manager=self.budget_manager,
                            budget_tokens=int(
                                counterparty_cfg.budget_tokens
                                if counterparty_cfg is not None
                                else 3000
                            ),
                            event_queue=self.event_queue,
                        )
                    logger.info(
                        "learning_enabled",
                        subsystem="counterparty",
                        profiler_active=counterparty_profiler is not None,
                    )
                # SynthesizedSkillStore + SkillSynthesizer +
                # SkillClusteringScheduler.  Store needs the DB; the
                # synthesizer also needs an LLM; the scheduler is opt-in.
                skill_synthesis_enabled = (
                    bool(skill_synth_cfg.enabled)
                    if skill_synth_cfg is not None
                    else True
                )
                skill_synthesizer: Any = None
                skill_scheduler: Any = None
                if skill_synthesis_enabled and isinstance(self.storage, Database):
                    from crewlet.learning.skill_scheduler import (
                        SkillClusteringScheduler,
                    )
                    from crewlet.learning.skill_synthesizer import SkillSynthesizer
                    from crewlet.learning.synthesized_skill_store import (
                        SynthesizedSkillStore,
                    )

                    self._synthesized_skill_store = SynthesizedSkillStore(self.storage)
                    if self._llm_providers:
                        skill_synthesizer = SkillSynthesizer(
                            llm_providers=self._llm_providers,
                            store=self._synthesized_skill_store,
                            budget_manager=self.budget_manager,
                            budget_tokens=int(
                                skill_synth_cfg.budget_tokens
                                if skill_synth_cfg is not None
                                else 4000
                            ),
                            max_skills_per_agent=int(
                                skill_synth_cfg.max_skills_per_agent
                                if skill_synth_cfg is not None
                                else 50
                            ),
                            duplicate_jaccard_threshold=float(
                                skill_synth_cfg.duplicate_jaccard_threshold
                                if skill_synth_cfg is not None
                                else 0.7
                            ),
                            event_queue=self.event_queue,
                            # Pass the store so the synthesizer can stamp
                            # ``consolidated_into_skill_id`` on the source
                            # episodes -- the lifecycle worker uses that
                            # to drop them after the configured grace.
                            episode_store=self._episode_store,
                        )
                        scheduler_enabled = (
                            bool(skill_synth_cfg.scheduler_enabled)
                            if skill_synth_cfg is not None
                            else False
                        )
                        if scheduler_enabled and self._episode_store is not None:
                            # PromotionSynthesizer drafts a knowledge-
                            # base page (under the unit's ``Auto-Drafted
                            # Skills`` parent) instead of writing a
                            # unit-scope row.  Requires an active
                            # knowledge backend (Confluence or Plane
                            # transport); without one no page writer
                            # exists, the synthesizer is left out, and
                            # the scheduler's promotion pass is a no-op.
                            from crewlet.learning.skill_synthesizer import (
                                PromotionSynthesizer,
                            )

                            promotion_enabled_cfg = (
                                bool(skill_promo_cfg.enabled)
                                if skill_promo_cfg is not None
                                else True
                            )
                            promotion_synth: Any = None
                            page_writer = self._build_promotion_page_writer()
                            if promotion_enabled_cfg and page_writer is not None:
                                promotion_synth = PromotionSynthesizer(
                                    llm_providers=self._llm_providers,
                                    page_writer=page_writer,
                                    org=self.org,
                                    budget_tokens=int(
                                        skill_promo_cfg.budget_tokens
                                        if skill_promo_cfg is not None
                                        else 4000
                                    ),
                                    event_queue=self.event_queue,
                                )
                            skill_scheduler = SkillClusteringScheduler(
                                synthesizer=skill_synthesizer,
                                episode_store=self._episode_store,
                                agent_pool=self.agent_pool,
                                organization=self.org,
                                concurrency=self.concurrency,
                                event_queue=self.event_queue,
                                interval_seconds=int(
                                    skill_synth_cfg.scheduler_interval_seconds
                                    if skill_synth_cfg is not None
                                    else 3600
                                ),
                                cluster_window_hours=int(
                                    skill_synth_cfg.cluster_window_hours
                                    if skill_synth_cfg is not None
                                    else 168
                                ),
                                cluster_min_size=int(
                                    skill_synth_cfg.cluster_min_size
                                    if skill_synth_cfg is not None
                                    else 3
                                ),
                                cluster_jaccard_threshold=float(
                                    skill_synth_cfg.cluster_jaccard_threshold
                                    if skill_synth_cfg is not None
                                    else 0.6
                                ),
                                episode_fetch_limit=int(
                                    skill_synth_cfg.episode_fetch_limit
                                    if skill_synth_cfg is not None
                                    else 200
                                ),
                                promotion_synthesizer=promotion_synth,
                                synthesized_skill_store=self._synthesized_skill_store,
                                promotion_enabled=promotion_enabled_cfg,
                                promotion_min_sibling_count=int(
                                    skill_promo_cfg.min_sibling_count
                                    if skill_promo_cfg is not None
                                    else 3
                                ),
                                promotion_jaccard_threshold=float(
                                    skill_promo_cfg.jaccard_threshold
                                    if skill_promo_cfg is not None
                                    else 0.6
                                ),
                                claim_duty=lambda: self.claim_worker_duty(
                                    "skill-clustering"
                                ),
                            )
                    logger.info(
                        "learning_enabled",
                        subsystem="skill_synthesis",
                        synthesizer_active=skill_synthesizer is not None,
                        scheduler_active=skill_scheduler is not None,
                    )

                # SkillRefiner + refine_skill tool.  Requires
                # the store; the refiner additionally needs an LLM.
                skill_refiner: Any = None
                if self._synthesized_skill_store is not None:
                    from crewlet.learning.tools import register_refine_skill_tool

                    register_refine_skill_tool(
                        self.tool_registry,
                        self._synthesized_skill_store,
                        max_body_chars=int(
                            skill_refine_cfg.max_body_chars
                            if skill_refine_cfg is not None
                            else 20000
                        ),
                        max_versions_kept=int(
                            skill_refine_cfg.max_versions_kept
                            if skill_refine_cfg is not None
                            else 10
                        ),
                    )
                    refinement_enabled = (
                        bool(skill_refine_cfg.enabled)
                        if skill_refine_cfg is not None
                        else True
                    )
                    if refinement_enabled and self._llm_providers:
                        from crewlet.learning.skill_refiner import SkillRefiner

                        skill_refiner = SkillRefiner(
                            llm_providers=self._llm_providers,
                            store=self._synthesized_skill_store,
                            budget_manager=self.budget_manager,
                            budget_tokens=int(
                                skill_refine_cfg.budget_tokens
                                if skill_refine_cfg is not None
                                else 3000
                            ),
                            max_body_chars=int(
                                skill_refine_cfg.max_body_chars
                                if skill_refine_cfg is not None
                                else 20000
                            ),
                            max_versions_kept=int(
                                skill_refine_cfg.max_versions_kept
                                if skill_refine_cfg is not None
                                else 10
                            ),
                            auto_refine_on_success=bool(
                                skill_refine_cfg.auto_refine_on_success
                                if skill_refine_cfg is not None
                                else True
                            ),
                            auto_refine_on_failure=bool(
                                skill_refine_cfg.auto_refine_on_failure
                                if skill_refine_cfg is not None
                                else True
                            ),
                            event_queue=self.event_queue,
                        )
                    logger.info(
                        "learning_enabled",
                        subsystem="skill_refinement",
                        refiner_active=skill_refiner is not None,
                    )

                if (
                    persist_decider_enabled
                    or counterparty_profiler is not None
                    or skill_synthesizer is not None
                    or skill_refiner is not None
                ) and self._llm_providers:
                    self._reflect_engine = ReflectEngine(
                        event_queue=self.event_queue,
                        llm_providers=self._llm_providers,
                        organization=self.org,
                        persist_decider_enabled=persist_decider_enabled,
                        persist_budget_tokens=persist_budget,
                        diary=self._agent_diary,
                        counterparty_profiler=counterparty_profiler,
                        skill_synthesizer=skill_synthesizer,
                        skill_scheduler=skill_scheduler,
                        skill_refiner=skill_refiner,
                        episode_store=self._episode_store,
                        single_turn_min_tool_calls=int(
                            skill_synth_cfg.min_tool_calls
                            if skill_synth_cfg is not None
                            else 5
                        ),
                        concurrency=self.concurrency,
                        budget_manager=self.budget_manager,
                    )
                logger.info(
                    "learning_enabled",
                    subsystem="reflect",
                    persist_decider=(self._reflect_engine is not None),
                )

                # Episode lifecycle worker.  Drains
                # the episodes table by dropping low-value rows and
                # LLM-compacting clusters of similar routine turns into
                # ``kind='compacted'`` summaries.  Trigger is write-side
                # (EpisodeStore publishes CompactionRequested when the
                # per-agent threshold is crossed); this worker
                # subscribes to that event.  Requires a real
                # EpisodeStore + at least one LLM provider.
                if self._episode_store is not None and self._llm_providers:
                    from crewlet.learning.episode_lifecycle import (
                        EpisodeLifecycleWorker,
                    )

                    cfg = episode_lifecycle_cfg
                    self._episode_lifecycle_worker = EpisodeLifecycleWorker(
                        event_queue=self.event_queue,
                        budget_manager=self.budget_manager,
                        episode_store=self._episode_store,
                        llm_providers=self._llm_providers,
                        organization=self.org,
                        agent_pool=self.agent_pool,
                        non_terminal_max_age_days=int(
                            cfg.non_terminal_max_age_days if cfg else 14
                        ),
                        consolidated_grace_days=int(
                            cfg.consolidated_grace_days if cfg else 30
                        ),
                        compaction_min_age_days=int(
                            cfg.compaction_min_age_days if cfg else 30
                        ),
                        compaction_min_cluster_size=int(
                            cfg.compaction_min_cluster_size if cfg else 3
                        ),
                        compaction_jaccard_threshold=float(
                            cfg.compaction_jaccard_threshold if cfg else 0.6
                        ),
                        compaction_batch_size=int(
                            cfg.compaction_batch_size if cfg else 200
                        ),
                        compaction_budget_tokens=int(
                            cfg.compaction_budget_tokens if cfg else 4000
                        ),
                        compacted_max_age_days=int(
                            cfg.compacted_max_age_days if cfg else 0
                        ),
                        exemplar_count=int(cfg.exemplar_count if cfg else 2),
                    )
                    logger.info("learning_enabled", subsystem="episode_lifecycle")

                # SkillCuratorWorker: ages out unused
                # synthesized skills (active → stale → archived) so
                # the Plan-phase prefetch doesn't accumulate dead
                # weight. Needs the SynthesizedSkillStore; mirrors
                # the EpisodeLifecycleWorker's start/stop wiring.
                curator_cfg = (
                    getattr(lrn_cfg, "skill_curator", None)
                    if lrn_cfg is not None
                    else None
                )
                if (
                    self._synthesized_skill_store is not None
                    and curator_cfg is not None
                    and curator_cfg.enabled
                ):
                    from crewlet.learning.skill_curator import SkillCuratorWorker

                    self._skill_curator_worker = SkillCuratorWorker(
                        store=self._synthesized_skill_store,
                        event_queue=self.event_queue,
                        interval_hours=int(curator_cfg.interval_hours),
                        stale_after_days=int(curator_cfg.stale_after_days),
                        archive_after_days=int(curator_cfg.archive_after_days),
                        claim_duty=lambda: self.claim_worker_duty("skill-curator"),
                    )
                    logger.info("learning_enabled", subsystem="skill_curator")
            else:
                logger.info(
                    "learning_reflect_disabled",
                    reason="disabled",
                    reflect_enabled=reflect_enabled,
                )

        # 6.9 Scheduler — role/unit-scoped cron-style task dispatch.
        # Auto-enabled when scheduling is on, the org declares schedules,
        # and a database is available (the ``scheduled_runs`` ledger backs
        # at-most-once delivery across restarts).  Reads the live org each
        # tick via ``org_provider`` so ``reload_config`` hot-reload picks
        # up added/removed schedules without a restart.
        from crewlet.config import SchedulingConfig
        from crewlet.schedule import (
            ScheduledRunStore,
            Scheduler,
            count_schedules,
            has_schedules,
        )

        # Fall back to the model's own defaults rather than re-encoding
        # each literal here (keeps one source of truth for the defaults).
        sched_cfg = self._scheduling_config or SchedulingConfig()
        if sched_cfg.enabled and has_schedules(self.org):
            if isinstance(self.storage, Database):
                self._scheduler = Scheduler(
                    event_queue=self.event_queue,
                    org_provider=lambda: self.org,
                    store=ScheduledRunStore(self.storage),
                    default_timezone=sched_cfg.default_timezone,
                    tick_seconds=sched_cfg.tick_seconds,
                    jitter_seconds=sched_cfg.jitter_seconds,
                    catchup_min_seconds=sched_cfg.catchup_min_seconds,
                    catchup_max_seconds=sched_cfg.catchup_max_seconds,
                    admits=self.admits_triggers,
                    claim_duty=lambda: self.claim_worker_duty("scheduler"),
                )
                logger.info("scheduler_enabled", schedules=count_schedules(self.org))
            else:
                logger.warning(
                    "scheduler_disabled_no_database",
                    schedules=count_schedules(self.org),
                    hint="role/unit schedules require a PostgreSQL database "
                    "for at-most-once delivery",
                )

        # 7. Set up the turn engine with all subsystem references
        logger.info(
            "start_step",
            step="7/8",
            action="setup_turn_engine",
            llm_providers=len(self._llm_providers),
        )
        if self._llm_providers:
            self._build_turn_engine(
                summarize_enabled=summarize_enabled,
                summarize_max_tokens=summarize_max_tokens,
            )

        # 7.5 Claim seats. The host's first sweep runs inline, so by the
        # time ``start`` returns this node has established (and attached)
        # its share — the same guarantee the old spawn-everything step
        # gave, minus the seats that are not ours.
        logger.info("start_step", step="7.5/8", action="claim_seats")
        if self._seat_host is not None and not self._seat_host_started:
            await self._seat_host.start()
            self._seat_host_started = True
            self._start_loop_watchdog()

        # Notification service wakes agents via the EventQueue inbox
        # subscriptions above; no direct engine coupling is needed.
        try:
            await self.notification_service.start()
        except Exception as exc:
            logger.error("notification_service_start_failed", error=str(exc))

        # Start the ReflectEngine after the inbox subscriptions land
        # so its turn-completion consumer group registers without
        # fighting for the same connection slot.
        if self._reflect_engine is not None:
            try:
                await self._reflect_engine.start()
            except Exception as exc:
                logger.error("reflect_engine_start_failed", error=str(exc))

        # Episode lifecycle worker subscribes to its own topic
        # (compaction_requested); same ordering rationale as ReflectEngine.
        if self._episode_lifecycle_worker is not None:
            try:
                await self._episode_lifecycle_worker.start()
            except Exception as exc:
                logger.error("episode_lifecycle_worker_start_failed", error=str(exc))

        # SkillCuratorWorker is interval-driven (default 24h);
        # mirror the lifecycle-worker start pattern. Best-effort: a
        # start failure is logged but never aborts engine startup.
        if self._skill_curator_worker is not None:
            try:
                await self._skill_curator_worker.start()
            except Exception as exc:
                logger.error("skill_curator_worker_start_failed", error=str(exc))

        # Scheduler — interval-driven cron-style dispatch. Mirrors
        # the lifecycle-worker start pattern; a start failure is logged
        # but never aborts engine startup.
        if self._scheduler is not None:
            try:
                await self._scheduler.start()
            except Exception as exc:
                logger.error("scheduler_start_failed", error=str(exc))

        # Budget reporter — publishes the live token meters on a tick
        # when they move.  A producer like the scheduler, and equally
        # best-effort: losing it costs a dashboard panel, never a turn.
        if self._budget_reporter is not None:
            try:
                await self._budget_reporter.start()
            except Exception as exc:
                logger.error("budget_reporter_start_failed", error=str(exc))

        # Retention sweeps for the short-horizon tables. Built here
        # rather than in the Tier-B cascade because it depends only on
        # the storage backend, and skipped entirely without one: the
        # memory twins prune themselves inline, since a process-local
        # dict dies with the process.
        self._build_a2a_channel_store()
        self._build_turn_completion_store()
        self._start_maintenance_worker()
        if self._maintenance_worker is not None:
            try:
                await self._maintenance_worker.start()
            except Exception as exc:
                logger.error("maintenance_worker_start_failed", error=str(exc))

        # Sandbox coordinator — subscribes the
        # started/completed control topics and re-attaches to in-flight
        # detached runs after a restart. Best-effort: a failure is logged
        # but never aborts engine startup.
        await self._start_sandbox_coordinator()

        # Tool Skills boot-time populate.  Runs as a
        # background task with a bounded backoff retry (the compose
        # boot race: the knowledge backend may accept connections a few
        # seconds after the engine) off the active backend's worker
        # (ToolSkillSyncWorker on Confluence, PlaneSkillSyncWorker on
        # Plane) -- the registry stays empty until a walk lands;
        # webhook events apply normally in the meantime.  MCP servers
        # are already up (started synchronously earlier in start()), so
        # the populated registry gets checked against the real tool
        # surface.
        self._kick_tool_skill_resync()

        # 8. Register and start extensions
        logger.info(
            "start_step",
            step="8/8",
            action="register_extensions",
            count=len(self._pending_extensions),
        )
        ext_ctx = self._build_extension_context()
        for ext in self._pending_extensions:
            await self._extension_manager.register(ext, ext_ctx)
        await self._extension_manager.start_all(ext_ctx)

        # 8.5 Set up auto-subscriptions
        logger.info("start_step", step="8.5/8", action="setup_subscriptions")
        from crewlet.events.subscriptions import setup_subscriptions

        # Handlers read the org per event (not a captured snapshot) so
        # hot reloads — including seat-kind flips — re-route correctly.
        self._cancel_deadline_timers = await setup_subscriptions(
            self.event_queue,
            lambda: self.org,
        )

        self._tier_b_done = True
        self._running = True

        await self.event_queue.publish(
            "crewlet.events.org_started",
            OrgStarted(source="engine", org_name=self.org.name),
        )
        logger.info("engine_started", org=self.org.name)

    async def _subscribe_agent_inbox(
        self, agent: AgentInstance, epoch: int = 0
    ) -> None:
        """Subscribe *agent*'s inbox with batched per-conversation delivery.

        Events that queue up while the agent is busy (or within the
        configured linger window) are drained together and partitioned
        by :func:`~crewlet.notifications.coalesce.conversation_key` —
        same-conversation notifications reach the handler as one batch,
        everything else as single-event batches.  The shared
        ``BatchOptions`` instance is mutated in place by live config
        reloads.

        Attaching a consumer is what makes this node the seat's owner in
        practice, so this RAISES rather than silently skipping when asked
        to attach something it believes attached. The old guard returned
        quietly, which meant a seat whose release forgot to clear the set
        came back owned in the lease table and dark in the process — the
        absence of a log line being the only signal.

        ``epoch`` records which claim this attachment belongs to, so a
        release can tell "the consumer I attached" from "a consumer a
        later claim attached".
        """
        if not agent.handle:
            raise ValueError(
                "cannot attach an inbox for an empty handle — a seat with "
                "no handle is not routable (see crewlet.queue.topics)"
            )
        if agent.handle in self._subscribed_inboxes:
            raise RuntimeError(
                f"inbox for seat {agent.handle!r} is already attached at "
                f"epoch {self._subscribed_inboxes[agent.handle]}; attaching "
                f"twice would put two competing consumers on one seat"
            )
        started = time.monotonic()
        await self.event_queue.subscribe_batch(
            topic=agent_inbox_topic(agent.handle),
            group=agent_inbox_group(agent.handle),
            handler=self._make_agent_handler(agent),
            batch_key=conversation_key,
            options=self._inbox_batch_options,
        )
        self._subscribed_inboxes[agent.handle] = epoch
        logger.info(
            "inbox_attached",
            seat=agent.handle,
            epoch=epoch,
            elapsed_ms=round((time.monotonic() - started) * 1000, 1),
        )

    async def _detach_agent_inbox(self, handle: str) -> None:
        """Stop consuming a seat's inbox, leaving the subscription.

        Raises :class:`~crewlet.seat.host.SeatReleaseError` when the
        detach cannot be proven, so the caller fails closed and keeps the
        lease rather than handing a seat to a peer while this process may
        still be consuming it.
        """
        from crewlet.seat.host import SeatReleaseError

        epoch = self._subscribed_inboxes.pop(handle, None)
        started = time.monotonic()
        try:
            await self.event_queue.detach(
                agent_inbox_topic(handle), agent_inbox_group(handle)
            )
        except Exception as exc:
            # Put it back: this process is still attached, and the
            # bookkeeping must say so.
            if epoch is not None:
                self._subscribed_inboxes[handle] = epoch
            raise SeatReleaseError(
                f"could not detach the inbox consumer for seat {handle!r}: {exc}"
            ) from exc
        logger.info(
            "inbox_detached",
            seat=handle,
            epoch=epoch,
            elapsed_ms=round((time.monotonic() - started) * 1000, 1),
        )

    def _make_agent_handler(
        self, agent: AgentInstance
    ) -> Callable[[list[Event]], Awaitable[None]]:
        """Create a per-agent batch callback that dispatches by event type.

        The returned handler is subscribed to the agent's inbox topic on
        the EventQueue via :meth:`_subscribe_agent_inbox` and receives
        one same-conversation partition per call.  A multi-event
        partition is always external notifications for one conversation
        (every other inbox event type keys uniquely and so arrives
        alone); it is merged into ONE digest trigger so the agent runs
        one turn instead of N.  Single-event partitions dispatch exactly
        as they did before batching.  Concurrency is managed inside
        ``execute_turn()`` (which acquires the ConcurrencyController
        semaphore internally).
        """

        async def handle(events: list[Event]) -> None:
            from crewlet.agent.turn import SeatLost, ShutdownDraining

            if not events:
                return
            # Same-id dedupe FIRST, before any parking branch: at-least-
            # once delivery — and the requeue machinery's own republish
            # edges (a publish that timed out client-side but landed, a
            # partial requeue followed by a partition NAK) — can put two
            # copies of one event in the same drain. Identical ids mean
            # identical payloads by construction, so dropping the extras
            # is the one always-safe dedupe.
            #
            # It runs before the parking branches because those
            # REPUBLISH: deduping afterwards meant every park pushed the
            # duplicates back onto the topic, so copies multiplied across
            # shed / sandbox / park cycles instead of holding steady, and
            # were only ever collapsed by a drain that finally got
            # through.
            events = self._dedupe_inbox_events(agent, events)
            if not events:
                return
            # Ownership. This node consumes the seat only while it holds
            # the seat's lease; the branch is what stops a node that has
            # lost it from starting work a peer may already be doing.
            #
            # ``DeferDelivery``, not a requeue: a requeue sends these to
            # the topic tail while the successor replays its prefetched
            # siblings from the head, which reorders the conversation.
            # And not a NAK, which would spend the dead-letter budget on
            # messages nothing is wrong with. Leave them unacked and stop
            # consuming — the successor gets them in order, at
            # redeliveryCount 0.
            if not self._may_serve_seat(agent.handle):
                # Tell the host the consumer is stopping, so the next
                # successful renew resumes it. Freshness refuses inside
                # an ordinary heartbeat window on a perfectly healthy
                # node, so without this a seat goes deaf for the life of
                # the process the first time a batch lands in that
                # window. See ``SeatHost.note_delivery_deferred``.
                if self._seat_host is not None:
                    self._seat_host.note_delivery_deferred(agent.handle)
                raise DeferDelivery(f"seat {agent.handle!r} is not owned here")
            # No turn engine yet (booted with zero LLM providers): PARK the
            # events instead of consuming-and-dropping them.  Pause the
            # topic first so the requeued copies buffer on the queue, then
            # requeue + ack; ``_ensure_turn_engine_after_providers`` resumes
            # every inbox once the first provider lands and the engine can
            # actually run turns.
            if self.turn_engine is None:
                await self.event_queue.pause_topic(
                    agent_inbox_topic(agent.handle),
                    agent_inbox_group(agent.handle),
                    reason=_NO_TURN_ENGINE_PAUSE_REASON,
                )
                await self._requeue_inbox_events(agent, events)
                return
            # Busy on a detached sandbox job: park (requeue + ack) rather
            # than hold the delivery — the job can run for hours, far past
            # any broker ack window.  The coordinator paused the topic on
            # kick-off (and resumes it at completion), so the requeued
            # copies wait on the queue; this branch only catches deliveries
            # already in flight when the pause landed.
            if agent.state == AgentState.AWAITING_SANDBOX:
                await self._requeue_inbox_events(agent, events)
                return
            # Config posture: this node cannot apply an epoch its peers
            # have, so it must not start NEW work under a stale company.
            # Defer — leave the delivery unacked and stop consuming this
            # inbox — rather than requeue it, and never NAK (three
            # redeliveries at 1 s dead-letter a perfectly healthy event,
            # and the node may be shedding for minutes).
            #
            # Requeue was wrong here twice over. A shed RELEASES this
            # node's seats, and a release is fenced, so this is exactly
            # the case the fenced rule already covers: republishing sends
            # the events to the topic tail while the successor replays
            # its prefetched siblings from the head, which reorders a
            # conversation. And a requeue lands back on a topic this node
            # is still attached to — so if the release that follows fails
            # (an undead seat, a handoff exception) the copy comes
            # straight back, is shed again, and republished again, at
            # whatever rate the broker will serve. Deferring cannot spin:
            # the consumer stops after the first one.
            #
            # This sits AFTER the AWAITING_SANDBOX branch on purpose: a
            # seat mid-sandbox is already parked there, so a clarification
            # answer reaching a shedding node behaves exactly as it does
            # on a healthy one.  The sandbox *resume* never passes through
            # here at all — the coordinator dispatches it directly — which
            # is what keeps the gate from stranding a run whose pending
            # row is already flipped and whose box is already collected.
            if not self.admits_triggers():
                logger.info(
                    "inbox_events_shed",
                    agent_handle=agent.handle,
                    posture=str(self.posture),
                    count=len(events),
                )
                # The resume edge, same as the other two defer paths: a
                # posture that recovers without the seat ever changing
                # hands leaves nothing else to un-quiesce the inbox.
                if self._seat_host is not None:
                    self._seat_host.note_delivery_deferred(agent.handle)
                raise DeferDelivery(f"config posture {self.posture}")
            # Re-entrancy guard (memory backend): a publish to this agent's
            # OWN inbox from inside its running turn dispatches inline in
            # the same task — waiting for the agent there would deadlock on
            # ourselves.  Requeue from a fresh task instead; that later
            # delivery waits for the turn like any other event.
            if (
                agent.state == AgentState.WORKING
                and agent.working_task is asyncio.current_task()
            ):
                requeue = asyncio.create_task(
                    self._requeue_inbox_events(agent, list(events))
                )
                self._requeue_tasks.add(requeue)
                requeue.add_done_callback(self._requeue_tasks.discard)
                return
            # Already worked? Read the completion ledger HERE — after
            # every parking branch, so a parked partition is not marked
            # done, and BEFORE coalescing, so recorded constituents drop
            # out of the partition and only the remainder merges. A
            # redelivery that overlaps a previous one partially (A+B,
            # then A+B+C) therefore skips A and B and runs C.
            events = await self._drop_worked_triggers(agent, events)
            if not events:
                return
            # The identity of the work about to run, bound for the whole
            # dispatch. Derived HERE because this is where the
            # constituent list exists — the coalesced digest that reaches
            # the turn is minted fresh on every merge, so a key taken
            # from it would differ on every redelivery and match nothing
            # (the same trap ``_record_worked_triggers`` documents).
            work_key = derive_work_key(
                [str(e.id) for e in events if e.type in _LEDGERED_INBOX_TYPES]
            )
            try:
                if len(events) > 1:
                    merged = await self._coalesce_inbox_events(agent, events)
                    if merged is not None:
                        # Through the SAME dispatch as a single event, not
                        # straight to ``_handle_notification``. Going direct
                        # skipped the sandbox clarification check, so a
                        # parked run whose answer happened to arrive
                        # alongside a follow-up message — which is what a
                        # threaded reply looks like — was handled as an
                        # unrelated message and its box stayed parked until
                        # the pause reaper killed it.
                        with bind_work_key(work_key):
                            await self._dispatch_inbox_event(agent, merged)
                        await self._record_worked_triggers(agent, events)
                        return
                    # Coalescing declined (heterogeneous partition —
                    # after the dedupe above, a genuine key-scheme bug)
                    # or crashed (malformed constituent).  Degrade to
                    # per-event semantics: requeue the tail FIRST, then
                    # dispatch the head.  Requeue-before-dispatch means
                    # a requeue failure NAKs the partition before any
                    # work ran — no completed turn is ever replayed by
                    # a later event's failure.  A partially-requeued
                    # tail can leave same-id copies behind after the
                    # NAK; the dedupe above collapses them on the next
                    # drain.
                    await self._requeue_inbox_events(agent, events[1:])
                    # The head alone ran, so the key is the head's alone.
                    with bind_work_key(
                        derive_work_key(
                            [
                                str(e.id)
                                for e in events[:1]
                                if e.type in _LEDGERED_INBOX_TYPES
                            ]
                        )
                    ):
                        await self._dispatch_inbox_event(agent, events[0])
                    await self._record_worked_triggers(agent, events[:1])
                    return
                with bind_work_key(work_key):
                    await self._dispatch_inbox_event(agent, events[0])
                await self._record_worked_triggers(agent, events)
            except ShutdownDraining:
                # The turn never started (engine draining) -- re-raise
                # so the queue NAKs the partition and the next boot runs
                # it from scratch (a coalesced partition redelivers all
                # its constituent messages together).
                logger.info(
                    "turn_deferred_for_shutdown",
                    agent_handle=agent.handle,
                    event_type=events[0].type,
                    event_count=len(events),
                )
                raise
            except SeatLost as exc:
                # The lease moved mid-turn. Defer, do not NAK: this is
                # not a failed handler, and spending the message's
                # dead-letter budget on a handoff would kill a healthy
                # event after enough of them. The seat's new owner picks
                # the delivery up, in order, at redeliveryCount 0.
                logger.warning(
                    "turn_abandoned_seat_lost",
                    agent_handle=agent.handle,
                    event_type=events[0].type,
                    event_count=len(events),
                )
                # Same edge the ownership branch above records, and for
                # the same reason: this deferral quiesces the consumer,
                # and the resume hook is edge-triggered on a set the
                # deferral would otherwise never enter. `SeatLost` is
                # not always a real handoff — the in-turn fence trips in
                # the ordinary freshness window too, and there the node
                # still holds the seat, so nothing detaches and nothing
                # resumes. The seat goes deaf on a healthy node.
                if self._seat_host is not None:
                    self._seat_host.note_delivery_deferred(agent.handle)
                raise DeferDelivery(str(exc)) from exc

        return handle

    async def _dispatch_inbox_event(self, agent: AgentInstance, event: Event) -> None:
        """Dispatch ONE inbox event by type — the pre-batching semantics.

        Both the single-event partition path and the degrade path call
        this directly, so there is exactly one ``ShutdownDraining``
        handler per delivery (in the batch handler above) and no
        re-entrant logging.
        """
        match event.type:
            case "task_assigned":
                # ConcurrencyController is acquired inside
                # the turn engine -- no double-acquire here.
                if self.turn_engine is not None:
                    # Scheduled tasks carry a hard wall-clock
                    # cap so a runaway run can't monopolise the
                    # runner; enforced inside the turn engine.
                    deadline = _scheduled_deadline(event)
                    await self.turn_engine.run_turn(
                        agent,
                        event=event,
                        org=self.org,
                        deadline_seconds=deadline,
                    )
            case "a2a_request" | "a2a_message":
                await self._handle_a2a(agent, event)
            case "notification" | "external_notification":
                # A reply on a conversation where a sandbox job is waiting
                # for an answer resumes the coding work instead of
                # being handled as an unrelated message.
                if (
                    self._sandbox_coordinator is not None
                    and await self._sandbox_coordinator.try_resume_from_answer(
                        agent, event
                    )
                ):
                    return
                await self._handle_notification(agent, event)
            case "task_created" | "task_completed" | "task_delegated":
                # Informational events routed to this agent's
                # inbox for awareness (e.g. lead notified of new
                # task, manager notified of completion).  Logged
                # at debug level; the agent processes these as
                # context on its next turn.
                logger.debug(
                    "inbox_notification",
                    agent_handle=agent.handle,
                    event_type=event.type,
                )
            case _:
                logger.warning(
                    "unknown_inbox_event",
                    agent_handle=agent.handle,
                    event_type=event.type,
                )

    async def _coalesce_inbox_events(
        self, agent: AgentInstance, events: list[Event]
    ) -> Event | None:
        """Merge a same-conversation partition into one digest trigger.

        Returns ``None`` when the partition can't be merged: not
        uniformly external notifications (never expected — conversation
        keys are type-namespaced — so this signals a key-scheme bug and
        logs at error level), or the merge itself raised (a malformed
        constituent, e.g. an extension event with a naive timestamp).
        Either way the caller degrades to per-event dispatch — a
        partition must never be dropped or dead-lettered wholesale
        because one constituent broke the digest.  Publishes a
        ``NotificationsCoalesced`` telemetry event best-effort.
        """
        from crewlet.events.types import ExternalNotification, NotificationsCoalesced

        if not all(isinstance(e, ExternalNotification) for e in events):
            logger.error(
                "coalesce_partition_not_notifications",
                agent_handle=agent.handle,
                event_types=[e.type for e in events],
            )
            return None
        notifications: list[ExternalNotification] = list(events)  # type: ignore[arg-type]
        try:
            merged = coalesce_notifications(notifications)
        except Exception:
            logger.exception(
                "inbox_coalesce_failed",
                agent_handle=agent.handle,
                event_count=len(notifications),
            )
            return None
        key = conversation_key(merged)
        logger.info(
            "inbox_events_coalesced",
            agent_handle=agent.handle,
            conversation_key=key,
            count=len(notifications),
            source=merged.notification_source,
        )
        try:
            timestamps = [e.timestamp for e in notifications]
            await self.event_queue.publish(
                "crewlet.events.notifications_coalesced",
                NotificationsCoalesced(
                    source=agent.handle,
                    agent_handle=agent.handle,
                    conversation_key=key,
                    notification_source=merged.notification_source,
                    count=len(notifications),
                    first_at=min(timestamps).isoformat(),
                    last_at=max(timestamps).isoformat(),
                    trace_id=merged.trace_id,
                    span_id=merged.span_id,
                    parent_span_id=merged.parent_span_id,
                ),
            )
        except Exception:
            logger.exception("notifications_coalesced_publish_failed")
        return merged

    async def _requeue_inbox_events(
        self, agent: AgentInstance, events: list[Event]
    ) -> None:
        """Republish *events* to the agent's inbox as independent messages.

        Used when a multi-event partition can't be handled as one
        digest: the caller requeues the tail BEFORE dispatching the
        head, so each tail event gets its own partition / ack lifecycle
        on a later drain and a requeue failure aborts the partition
        before any turn ran.  A republish failure raises — the queue
        NAKs the whole partition; events already requeued then exist
        twice (requeued copy + redelivered original), and the handler's
        same-id dedupe collapses them when they next arrive together.
        """
        topic = agent_inbox_topic(agent.handle)
        for event in events:
            await self.event_queue.publish(topic, event)
        logger.info(
            "inbox_events_requeued",
            agent_handle=agent.handle,
            count=len(events),
        )

    async def _handle_a2a(self, agent: AgentInstance, event: Event) -> None:
        """Handle an A2A channel request or reply for an agent.

        The message content arrives ON the event. It used to be read
        out of a per-channel ``asyncio.Queue``, which is process-local:
        the target of an ask wakes on the node that owns ITS seat, and
        that is rarely the node that opened the channel — so cross-node
        the queue was empty and the agent was told nobody had said
        anything yet. Reading the payload also makes the trigger
        re-runnable, which is what lets the completion ledger cover A2A
        at all: a drained queue could not be re-read.

        Handles both ``a2a_request`` (the opening brief) and
        ``a2a_message`` (a reply on an open channel).
        """
        if self.a2a_service is None:
            logger.warning("a2a_service_not_configured", agent_handle=agent.handle)
            return

        channel_id = event.payload.get("channel_id", "")
        # For a2a_request the other party is "requester"; for
        # a2a_message it is "sender".
        requester = event.payload.get("requester", "") or event.payload.get(
            "sender", ""
        )
        content = event.payload.get("content", "") or ""
        sender_role = event.payload.get("sender_role", "") or ""
        if not channel_id:
            logger.warning(
                "a2a_request_missing_channel",
                agent_handle=agent.handle,
            )
            return

        channel = await self.a2a_service.channels.get(channel_id)
        if channel is None:
            # No longer swallowed. With the state durable, "unknown"
            # genuinely means never opened — a typo'd or long-purged
            # channel id — rather than "opened on another node".
            logger.warning(
                "a2a_channel_unknown",
                agent_handle=agent.handle,
                channel_id=channel_id,
            )
            return

        # Only the party that did NOT open the channel answers on it.
        # Otherwise the requester's turn — itself triggered by the reply
        # — would answer the answer, and two agents would volley until
        # the delegation cap stopped them.
        #
        # Read from the CHANNEL, not from the event type: the record is
        # what both nodes agree on, and it settles the degenerate
        # requester-is-target case the same way on either.
        is_responder = agent.handle != channel.requester

        if is_responder and not channel.is_open:
            # An answer with nowhere to go. Closed means the
            # conversation ended — the reply already shipped, or the
            # idle sweep gave up on it — and either way nothing is
            # waiting on a second one.
            logger.warning(
                "a2a_channel_not_open",
                agent_handle=agent.handle,
                channel_id=channel_id,
                state=channel.state,
            )
            return
        # The requester's hop is NOT gated on that. The responder closes
        # the channel immediately after replying, so by the time the
        # reply reaches the asker the channel is closed every time —
        # refusing it here would drop the answer on the floor, which is
        # the exact failure A2A had before it had a reply path at all.
        # In-process the close raced the delivery and lost; on a real
        # broker it wins, so this only ever looked correct on the twin.

        logger.info(
            "a2a_channel_join",
            agent_handle=agent.handle,
            channel_id=channel_id,
            requester=requester,
            content_length=len(content),
        )

        if self.event_queue is not None:
            from crewlet.events.types import A2AMessageDelivered

            # Trace context comes from the originating event so sent and
            # delivered group into one dashboard trace.
            delivered_event = A2AMessageDelivered(
                source=agent.handle,
                channel_id=channel_id,
                recipient=agent.handle,
                sender=requester,
                message_count=1 if content else 0,
                total_content_length=len(content),
                trace_id=event.trace_id,
                span_id=event.span_id,
                parent_span_id=event.parent_span_id,
            )
            await self.event_queue.publish(
                "crewlet.events.a2a_message_delivered", delivered_event
            )

        role_tag = f" ({sender_role})" if sender_role else ""
        parts = [
            f"You received a direct agent-to-agent (A2A) message from '{requester}'.",
            f"A2A Channel: {channel_id}",
            "",
        ]
        if content:
            parts.append("**Message:**")
            parts.append(f"- **{requester}{role_tag}:** {content}")
        else:
            parts.append(f"'{requester}' opened an A2A channel with no message.")
        parts.append("")
        if is_responder:
            parts.extend(
                [
                    "*** INSTRUCTIONS ***",
                    "- Answer in your final response. Whatever you say"
                    f" is delivered to '{requester}' on this channel, and"
                    " the channel then closes — you do not need a tool"
                    " to reply, and there is no second round.",
                    "- This is a private channel between you"
                    f" and '{requester}' — not visible in Slack.",
                ]
            )
        else:
            parts.extend(
                [
                    "*** INSTRUCTIONS ***",
                    "- This is the answer to a question you asked."
                    " Act on it; the channel is now closed.",
                ]
            )

        task_description = "\n".join(parts)

        logger.info(
            "a2a_prompt_constructed",
            agent_handle=agent.handle,
            channel_id=channel_id,
            requester=requester,
            responder=is_responder,
            prompt_length=len(task_description),
        )

        if self.turn_engine is None:
            return
        answer = await self.turn_engine.run_turn(
            agent,
            task_description=task_description,
            event=event,
            org=self.org,
            a2a_context={
                "channel_id": channel_id,
                "requester": requester,
                "responder": is_responder,
            },
        )
        if is_responder:
            await self._answer_a2a(agent, channel_id, answer or "", event)

    async def _answer_a2a(
        self, agent: AgentInstance, channel_id: str, answer: str, event: Event
    ) -> None:
        """Deliver the turn's final text as the reply, then close.

        THE reply path, and until now there was none. ``a2a_ask``'s own
        description promises "they reply on the same channel", and the
        wake prompt told the agent to call ``send_a2a_message`` — a tool
        that does not exist and that ``tests/test_tools/test_builtin``
        asserts is not registered. So every ask delivered a brief and
        every answer went nowhere.

        The turn's answer IS the reply: no new LLM-facing tool, and no
        channel lifecycle for a model to manage and forget. One round
        trip, then closed — which is what "tight-loop / mechanical sync"
        means, and what stops two agents volleying.

        Failures here are logged, not raised: the turn already happened,
        and a delivery failure must not make the trigger look unhandled
        and replay the whole thing.
        """
        if self.a2a_service is None:
            return
        try:
            if answer.strip():
                await self.a2a_service.reply(
                    channel_id,
                    agent.handle,
                    answer,
                    sender_role=agent.role_name,
                    delegation_depth=event.delegation_depth,
                    delegation_chain=list(event.delegation_chain or []),
                    parent_turn_id=event.parent_turn_id,
                )
            else:
                logger.warning(
                    "a2a_answer_empty",
                    agent_handle=agent.handle,
                    channel_id=channel_id,
                )
        except Exception:
            logger.exception(
                "a2a_answer_failed",
                agent_handle=agent.handle,
                channel_id=channel_id,
            )
        finally:
            with contextlib.suppress(Exception):
                await self.a2a_service.close_channel(channel_id, closer=agent.handle)

    async def _handle_notification(self, agent: AgentInstance, event: Event) -> None:
        """Handle an external notification: wake the agent and run a turn.

        Concurrency is managed inside ``execute_turn()`` — no
        double-acquire at the handler level.
        """
        logger.info(
            "notification_received",
            agent_handle=agent.handle,
            event_type=event.type,
        )
        if self.turn_engine is not None:
            await self.turn_engine.run_turn(agent, event=event, org=self.org)

    @property
    def extensions(self) -> list[Extension]:
        """Get registered extensions."""
        return self._extension_manager.extensions

    def _build_extension_context(self) -> ExtensionContext:
        return ExtensionContext(
            event_queue=self.event_queue,
            agent_pool=self.agent_pool,
            execution_tracker=self.execution_tracker,
            tool_registry=self.tool_registry,
            role_mcp_tools=self._role_mcp_tools,
            storage=self.storage,
            notification_service=self.notification_service,
            org=self.org,
            observability=self.observability,
            debug=self.debug,
            node_id=self._node_id,
            # The same per-tick singleton the engine's own duties use.
            # Extensions run on every node, so a company-wide job an
            # extension does unconditionally is a job done N times.
            claim_duty=self.claim_worker_duty,
        )

    def _build_tools_data(self) -> list[dict[str, Any]]:
        """Build tool descriptions for the API, tagged with source and roles.

        Each entry contains name, description, source (builtin or
        mcp:<server>), and roles (list of role names that have access).
        """
        from crewlet.mcp.bridge import MCPToolWrapper

        # Collect all role names from the org
        all_role_names = [r.name for r in self.org.all_roles()]

        # Build a set of tool names that are per-role (not global)
        role_only_tools: dict[str, set[str]] = {}  # tool_name → {role, …}
        for role_name, mcp_tools in self._role_mcp_tools.items():
            for tool in mcp_tools:
                role_only_tools.setdefault(tool.name, set()).add(role_name)

        # Global tools from the registry — available to all roles
        # (unless overridden by per-role MCP tools for specific roles)
        tools_data: list[dict[str, Any]] = []
        seen: set[str] = set()

        for tool in self.tool_registry.list_tools():
            if tool.name in seen:
                continue
            seen.add(tool.name)

            source = (
                f"mcp:{tool._client.name}"
                if isinstance(tool, MCPToolWrapper)
                else "builtin"
            )

            # Global tools are available to all roles.
            tools_data.append(
                {
                    "name": tool.name,
                    "description": tool.description,
                    "source": source,
                    "roles": sorted(all_role_names),
                }
            )

        # Per-role MCP tools not already in the global registry
        for role_name, mcp_tools in self._role_mcp_tools.items():
            for tool in mcp_tools:
                if tool.name in seen:
                    continue
                seen.add(tool.name)

                if isinstance(tool, MCPToolWrapper):
                    # Instance names use "server::Role_Name" convention;
                    # strip the role suffix so tools group by base server.
                    base_name = tool._client.name.split("::")[0]
                    source = f"mcp:{base_name}"
                else:
                    source = "mcp"
                roles = sorted(role_only_tools.get(tool.name, {role_name}))

                tools_data.append(
                    {
                        "name": tool.name,
                        "description": tool.description,
                        "source": source,
                        "roles": roles,
                    }
                )

        logger.info("tools_data_built", total=len(tools_data))
        return tools_data

    async def _start_embedded_api(self) -> Any:
        """Start an embedded API server sharing the engine's EventQueue.

        Returns the uvicorn ``Server`` instance so the caller can
        request shutdown.

        Wires the same Tier A bootstrap + Tier B store the engine
        holds so the embedded app mounts ``/config/*`` with auth +
        subscribes its cached state to ``revision_activated`` events.
        Without this, an embedded-API deployment (memory queue or
        small footprint single-process) would have no ``/config/*``
        routes and the founder couldn't edit the company live.
        """
        import uvicorn

        from crewlet.api.app import attach_config_refresh, create_app
        from crewlet.api.runtime import EngineNodeRuntime
        from crewlet.api.streaming import StreamService

        bootstrap = getattr(self, "_bootstrap", None)
        store = getattr(self, "_company_config_store", None)

        # Subscribe the stream service to the BROADCAST event stream —
        # the same wiring ``crewlet run api`` uses.  The old embedded
        # path registered a publish listener instead, which fires only on
        # this process's own publishes: correct for one process, and
        # structurally unable to see a peer's events.  Unifying on the
        # broadcast path is what makes the two deployments the same code
        # rather than the same surface with two implementations.
        #
        # Cached on ``self`` so repeated _start_embedded_api calls (test
        # harnesses, future restart paths) don't stack subscriptions.
        stream = getattr(self, "_stream", None)
        if stream is None:
            stream = StreamService()
            try:
                await self.event_queue.subscribe_stream(
                    "crewlet.events.>", stream.ingest
                )
            except Exception:
                logger.exception("embedded_api_stream_subscribe_failed")
            self._stream = stream

        # Webhook secrets, roles, org, schedules and tools are NOT passed
        # in: ``attach_config_refresh`` derives every one of them from the
        # active revision, on this path exactly as on the standalone one.
        app = create_app(
            event_queue=self.event_queue,
            event_store=self._event_store,
            database=self.storage,
            sandbox_otel_receiver=getattr(self, "_sandbox_otel_receiver", None),
            bootstrap=bootstrap,
            company_config_store=store,
            runtime=EngineNodeRuntime(self),
            stream=stream,
        )
        if store is not None:
            # poll=False: this engine's reconcile loop already polls the
            # activation pointer and drives ``refresh_if_changed`` from
            # its tick.  A second loop in the same process would only let
            # the engine and its API disagree about the current epoch.
            self._api_refresher = await attach_config_refresh(app, poll=False)
        elif self._active_config is not None:
            # No config store — a programmatic embed, or an engine built
            # without a database. There is no revision to read and no
            # activation event to react to, but the app still needs its
            # roles, org, schedules and (above all) webhook secrets. Feed
            # the in-memory config through the SAME derivation the refresh
            # path uses, so there is one implementation with two sources
            # rather than two implementations.
            from crewlet.api.config_refresh import _apply_payload_to_app
            from crewlet.config_yaml import company_config_to_dict

            try:
                _apply_payload_to_app(app, company_config_to_dict(self._active_config))
            except Exception:
                logger.exception("embedded_api_state_prime_failed")

        class _SignalFreeServer(uvicorn.Server):
            """Uvicorn server that never touches process signal handlers.

            ``Server.serve()`` otherwise registers its own SIGINT/SIGTERM
            handlers (``capture_signals``), replacing the engine's: the
            first Ctrl+C would shut down only the dashboard — exactly
            when the operator wants it alive to watch the drain — and on
            exit uvicorn re-raises the captured signals, which the
            engine would count as phantom extra presses and escalate to
            a force-stop mid-drain.  In embedded mode the engine's
            ``run()`` owns the process signals; shutdown reaches this
            server via ``should_exit`` / ``force_exit`` in
            :meth:`Engine._stop_embedded_api`.
            """

            @contextlib.contextmanager
            def capture_signals(self):  # type: ignore[override]
                yield

        # Exposed so operators (and tests) can inspect what the embedded
        # API actually resolved — the state is derived now, not passed in.
        self._api_app = app

        uv_config = uvicorn.Config(
            app,
            host=self._api_host,
            port=self._api_port,
            log_level="debug" if self.debug else "info",
        )
        server = _SignalFreeServer(uv_config)
        self._api_server = server
        self._api_serve_task = asyncio.create_task(server.serve())
        logger.info(
            "embedded_api_started",
            host=self._api_host,
            port=self._api_port,
            config_routes_mounted=store is not None,
        )
        return server

    async def _stop_embedded_api(self) -> None:
        """Bring the embedded API server down after the engine stopped.

        Runs AFTER :meth:`stop` so the dashboard keeps serving through
        the whole drain.  Escalation: ask uvicorn for a graceful exit
        (``should_exit``); if open connections (e.g. a dashboard
        WebSocket that hasn't noticed the close) keep ``serve()`` from
        returning, flip ``force_exit``; finally cancel the serve task
        outright so shutdown can never park here.
        """
        server = self._api_server
        task = self._api_serve_task
        self._api_server = None
        self._api_serve_task = None
        if server is None or task is None or task.done():
            return

        logger.debug("stopping_embedded_api")
        server.should_exit = True
        done, _ = await asyncio.wait({task}, timeout=self._api_stop_graceful_timeout)
        if not done:
            logger.warning("embedded_api_graceful_exit_timeout", action="force_exit")
            server.force_exit = True
            done, _ = await asyncio.wait({task}, timeout=self._api_stop_force_timeout)
        if not done:
            logger.warning("embedded_api_force_exit_timeout", action="cancel")
            task.cancel()
        # Reap the task; ``return_exceptions`` swallows the
        # CancelledError / any serve() error so API teardown can't
        # fail the caller.
        await asyncio.gather(task, return_exceptions=True)
        logger.info("embedded_api_stopped")

    async def run(self) -> None:
        """Start the engine and block until a shutdown signal is received.

        When ``api_port`` is configured (> 0), an embedded API server is
        started in the same process sharing the engine's EventQueue.
        This is required for the memory queue backend where API and
        engine must live in the same process.

        Handles SIGINT and SIGTERM for shutdown with a three-tier
        escalation. This is the recommended way to run the engine in
        production::

            await engine.run()

        1. **First signal** — graceful shutdown: event delivery pauses,
           in-flight agent turns run to completion, the embedded
           dashboard stays up so the drain is observable, then
           everything tears down in order.
        2. **Second signal** — force stop: all asyncio tasks are
           cancelled (in-flight turns are NAK'd for redelivery on the
           next boot) and a fast best-effort cleanup runs.
        3. **Third signal** — hard exit: ``os._exit(1)``, no cleanup.
           The escape hatch for a wedged event loop.

        The per-tier console notices are best-effort: each press
        schedules its shutdown action before printing, and a dead
        stderr (e.g. ``crewlet run 2>&1 | tee out``, where the same
        Ctrl+C kills ``tee`` and breaks the pipe) can neither prevent
        nor corrupt the shutdown — see :func:`_handle_shutdown_signal`.
        """
        stop_event = asyncio.Event()
        loop = asyncio.get_running_loop()
        signal_count = 0

        def _cancel_all_tasks() -> None:
            for task in asyncio.all_tasks(loop):
                task.cancel()

        def _schedule_graceful() -> None:
            # RuntimeError = loop already closed (signal raced teardown);
            # there is nothing left to stop, so dropping it is correct.
            with contextlib.suppress(RuntimeError):
                loop.call_soon_threadsafe(stop_event.set)

        def _schedule_force_cancel() -> None:
            with contextlib.suppress(RuntimeError):
                loop.call_soon_threadsafe(_cancel_all_tasks)

        def _on_signal(signum: int, _frame: Any) -> None:
            # Exactly ONE registration mechanism: ``signal.signal``.
            # Python-level handlers run at bytecode boundaries on the
            # main thread, so they fire even while the event loop is
            # blocked in synchronous code.  Registering the same signal
            # with BOTH ``loop.add_signal_handler`` and ``signal.signal``
            # would make every press fire twice — asyncio's
            # wakeup-fd machinery stays live when the Python-level
            # handler is swapped — so the very first Ctrl+C would be
            # counted as two signals and take the force-cancel path.
            nonlocal signal_count
            signal_count += 1
            # The escalation ladder schedules the shutdown action
            # BEFORE printing its console notice and never raises —
            # an exception escaping this handler would be re-raised
            # inside whatever frame the main thread was executing.
            _handle_shutdown_signal(
                signal_count,
                signum,
                schedule_graceful=_schedule_graceful,
                schedule_force_cancel=_schedule_force_cancel,
            )

        # Install signal handlers BEFORE start() so Ctrl+C works even
        # if start() blocks on network connections (Pulsar, DB, MCP
        # servers, etc.).  Previous dispositions are restored on exit.
        prev_handlers = {
            sig: signal.signal(sig, _on_signal)
            for sig in (signal.SIGINT, signal.SIGTERM)
        }
        logger.debug("signal_handlers_installed")

        try:
            # Run start() as a task so a shutdown signal can interrupt
            # it.  Without this, Ctrl+C during start() sets stop_event
            # but nothing cancels the hanging start() coroutine, making
            # it look like Ctrl+C does nothing.
            start_task = asyncio.create_task(self.start())
            wait_task = asyncio.create_task(stop_event.wait())

            done, _pending = await asyncio.wait(
                {start_task, wait_task},
                return_when=asyncio.FIRST_COMPLETED,
            )

            if start_task not in done:
                # Signal arrived during startup — cancel start and exit
                logger.warning("shutdown_during_startup")
                start_task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await start_task
                await self._force_stop()
                return

            wait_task.cancel()
            # Re-raise if start() failed
            start_task.result()

            # Start embedded API server if configured — and if this node
            # terminates inbound traffic at all. A node without the
            # ``ingress`` role that bound the port anyway would answer
            # webhooks the operator routed elsewhere, and would put a
            # dashboard on an address nothing is meant to reach.
            from crewlet.seat.placement import NodeRole

            if self._api_port > 0 and self.runs_role(NodeRole.INGRESS):
                await self._start_embedded_api()
            elif self._api_port > 0:
                logger.info(
                    "embedded_api_not_started",
                    node=self._node_id,
                    port=self._api_port,
                    reason="node.roles does not include 'ingress'",
                )

            # Block until shutdown signal
            if not stop_event.is_set():
                await stop_event.wait()

            # Graceful stop with the dashboard still serving: operators
            # watch the in-flight pill converge to 0 while agent turns
            # finish their rounds.  The API comes down only after the
            # engine is fully stopped.
            await self.stop()
            await self._stop_embedded_api()
        except asyncio.CancelledError:
            logger.warning("shutdown_cancelled_by_signal")
            # Force-cancel interrupted graceful stop — run a fast,
            # best-effort cleanup so child processes and connections
            # don't leak.
            await self._force_stop()
        finally:
            for sig, prev in prev_handlers.items():
                signal.signal(sig, prev)

    async def stop(self) -> None:
        """Graceful shutdown.

        The order is critical:

        0. Release every owned seat, one at a time, through the
           voluntary path — BEFORE pausing delivery. ``pause_delivery``
           is one-way and node-wide, so past it this node serves nothing
           while its heartbeat still renews every lease it holds: the
           seats are blackholed for the whole drain, unservable here and
           unclaimable anywhere else.
        1. Pause event delivery (no new messages dispatched to handlers)
        2. Stop work producers: deadline timers and the cron scheduler
           (no new auto-fired events), and flip the turn engine's
           shutdown gate so turns still parked at the concurrency
           semaphore are NAK'd for the next boot instead of starting
           fresh LLM rounds mid-drain
        3. Drain in-flight handlers indefinitely, logging
           ``drain_in_progress`` with the in-flight count every
           ``_drain_log_interval`` seconds.  Publishes still work, so a
           completing turn can emit its terminal ``TaskCompleted`` /
           ``TaskFailed`` before the queue is torn down.  The operator
           (second SIGINT/SIGTERM) -- or the host orchestrator (k8s
           ``terminationGracePeriodSeconds``, systemd
           ``TimeoutStopSec``) -- decides when "too long" is too long;
           that cancels this method via ``CancelledError`` and
           ``run()`` falls through to :meth:`_force_stop`.
           Duplicating the cutoff with our own timeout would be a
           guess at the orchestrator's grace period.
        4. Stop extensions
        5. Stop notification service, reflect engine, lifecycle workers
        6. Terminate agents and publish ``OrgStopped``
        7. Stop MCP servers and close LLM provider clients
        8. Stop event queue (final close) and close storage

        Each post-drain step has a short timeout (``_stop_step_timeout``)
        so a single teardown failure can't hang shutdown indefinitely.

        The embedded API server is NOT stopped here — ``run()`` keeps
        it serving through this whole method so the dashboard can show
        the drain, and brings it down afterwards via
        :meth:`_stop_embedded_api`.
        """
        if not self._running:
            return

        # Flip before anything else so /health (and the dashboard's
        # footer pill) reports the drain from its first moment;
        # ``is_running`` only flips once teardown completes.
        self._shutting_down = True

        step_timeout = self._stop_step_timeout

        async def _timed(label: str, coro: Any) -> None:
            """Run *coro* with a per-step timeout, logging failures."""
            try:
                await asyncio.wait_for(coro, timeout=step_timeout)
            except TimeoutError:
                logger.warning("stop_step_timeout", step=label, timeout=step_timeout)
            except Exception as exc:
                logger.error("stop_step_error", step=label, error=str(exc))

        logger.info("engine_stopping", org=self.org.name)

        # 0. Hand the seats back BEFORE pausing delivery.
        #
        # Order matters more than it looks. ``pause_delivery`` is
        # one-way and node-wide: past it, this node consumes nothing —
        # but its heartbeat keeps renewing every seat it holds, so those
        # seats are blackholed for the entire drain. No peer can take
        # them (the lease is live) and this node will not serve them
        # (delivery is paused). Releasing first lets peers pick each seat
        # up as it goes idle, which is the whole point of a graceful
        # drain.
        #
        # ``begin_drain`` also drops this node's presence lease, so peers
        # recompute their share over the nodes that will actually serve.
        # Stopped before the release, not after. Teardown is the one part
        # of the process that legitimately blocks the loop — reaping MCP
        # subprocesses, joining threads, tearing down sandboxes — and an
        # ``os._exit`` in the middle of it would abandon the seat release
        # that makes the drain graceful. A shutdown that hangs is a
        # SIGKILL away; a shutdown that exits without releasing costs
        # every peer a full TTL of dark seats.
        await self._stop_loop_watchdog()
        if self._seat_host is not None:
            try:
                await self._seat_host.begin_drain()
                await _timed("seat_release", self._seat_host.release_all())
                await _timed("seat_host", self._seat_host.stop())
            except Exception as exc:
                logger.error("seat_host_stop_failed", error=str(exc))
            self._seat_host_started = False

        # 1. Pause event delivery so no new turns start while we wait
        #    for in-flight ones to finish.  Publishes still work -- a
        #    turn that's already running can still emit TaskCompleted
        #    / TaskFailed before the queue is closed for good in step 9.
        logger.debug("pausing_event_delivery")
        try:
            await self.event_queue.pause_delivery()
        except Exception as exc:
            logger.error("pause_delivery_failed", error=str(exc))

        # 2. Stop the work producers so no further auto-fired events
        #    (deadline retries, scheduled runs, periodic syncs) queue up
        #    behind the paused queue.
        logger.debug("cancelling_background_tasks")
        # The config reconciler is a producer too: it can start a full
        # apply (respawning MCP children, rebuilding transports) that
        # would race the teardown below.  Cancelled first, before
        # anything it might touch begins to come down.
        await self._stop_control_plane()
        if self._cancel_deadline_timers is not None:
            self._cancel_deadline_timers()
            self._cancel_deadline_timers = None
        # A Tool Skills walk still in its retry backoff has nothing
        # left to seed for — cancel it so shutdown never waits out a
        # sleeping retry.
        if self._tool_skill_resync_task is not None:
            self._tool_skill_resync_task.cancel()
            self._tool_skill_resync_task = None
        # The cron scheduler is a producer, not a consumer:
        # stopped BEFORE the drain so it can't mark ``scheduled_runs``
        # rows fired into a queue this engine will never read again.
        if self._scheduler is not None:
            logger.debug("stopping_scheduler")
            await _timed("scheduler", self._scheduler.stop())
            self._scheduler = None
        # The budget reporter is a producer too, and its meters stop
        # meaning anything the moment this process does.
        if self._budget_reporter is not None:
            logger.debug("stopping_budget_reporter")
            await _timed("budget_reporter", self._budget_reporter.stop())
            self._budget_reporter = None
        # The sandbox waiter is a producer (fires SandboxRunCompleted):
        # stop it before the drain, same rationale as the scheduler.
        if self._sandbox_waiter is not None:
            logger.debug("stopping_sandbox_waiter")
            await _timed("sandbox_waiter", self._sandbox_waiter.stop())
            self._sandbox_waiter = None
        # Turns already past the concurrency gate finish their rounds;
        # turns still parked at the gate are NAK'd back to the broker
        # (see TurnEngine.begin_shutdown) so the drain length is the
        # length of the *running* rounds, not the whole backlog.
        if self.turn_engine is not None:
            self.turn_engine.begin_shutdown()

        # 3. Drain in-flight handlers indefinitely, with a progress
        #    heartbeat so an operator watching the console can tell a
        #    long LLM round from a hang.  A second signal (or SIGKILL
        #    from the host orchestrator) is what bails us out if a turn
        #    is genuinely stuck -- see the docstring.
        in_flight = self.in_flight_count
        if in_flight > 0:
            logger.info("draining_in_flight_handlers", in_flight=in_flight)
        while True:
            remaining = await self.event_queue.wait_for_handlers(
                timeout=self._drain_log_interval
            )
            if remaining == 0:
                break
            logger.info("drain_in_progress", in_flight=remaining)
        logger.info("drain_complete")

        # 4. Stop extensions
        logger.debug("stopping_extensions")
        ext_ctx = self._build_extension_context()
        await _timed("extensions", self._extension_manager.stop_all(ext_ctx))

        # 5. Stop notification service
        logger.debug("stopping_notification_service")
        if self.notification_service is not None:
            await _timed("notification_service", self.notification_service.stop())

        # 5.5. Stop the reflect engine
        if self._reflect_engine is not None:
            logger.debug("stopping_reflect_engine")
            await _timed("reflect_engine", self._reflect_engine.stop())

        # 5.6. Stop the skill curator worker
        if self._skill_curator_worker is not None:
            logger.debug("stopping_skill_curator_worker")
            await _timed(
                "skill_curator_worker",
                self._skill_curator_worker.stop(),
            )

        # 5.65. Stop the retention sweeps
        if self._maintenance_worker is not None:
            logger.debug("stopping_maintenance_worker")
            await _timed("maintenance_worker", self._maintenance_worker.stop())

        # 5.7. Stop the episode lifecycle worker
        if self._episode_lifecycle_worker is not None:
            logger.debug("stopping_episode_lifecycle_worker")
            await _timed(
                "episode_lifecycle_worker",
                self._episode_lifecycle_worker.stop(),
            )

        # 6. Terminate all agents
        logger.debug("terminating_agents")
        for agent in self.agent_pool.active_agents:
            await _timed(f"terminate_{agent.handle}", self.agent_pool.terminate(agent))

        await _timed(
            "publish_org_stopped",
            self.event_queue.publish(
                "crewlet.events.org_stopped",
                OrgStopped(source="engine", org_name=self.org.name),
            ),
        )

        # 7. Stop MCP servers
        logger.debug("stopping_mcp_servers")
        if self.mcp_bridge:
            await _timed("mcp_servers", self.mcp_bridge.stop_all())

        # 7.5. Close LLM provider HTTP clients
        logger.debug("closing_llm_providers")
        for provider in self._llm_providers.values():
            if hasattr(provider, "close"):
                await _timed("llm_provider", provider.close())

        # 8. Stop event queue, close storage
        logger.debug("stopping_event_queue")
        await _timed("event_queue", self.event_queue.stop())
        self._queue_started = False

        # Close every provider's pooled httpx clients before we
        # take down the storage / telemetry layers. Providers that
        # don't expose ``aclose`` (test fakes) are skipped silently.
        for key, provider in self._llm_providers.items():
            aclose = getattr(provider, "aclose", None)
            if aclose is None:
                continue
            logger.debug("closing_llm_provider", key=key)
            await _timed(f"llm_provider:{key}", aclose())

        logger.debug("closing_storage")
        if hasattr(self.storage, "close"):
            await _timed("storage", self.storage.close())

        # 9. Flush and shut down OpenTelemetry
        from crewlet.telemetry import shutdown_telemetry

        shutdown_telemetry()

        self._running = False
        logger.info("engine_stopped", org=self.org.name)

    async def _force_stop(self) -> None:
        """Best-effort fast cleanup after a force-cancel.

        Skips waiting for agents and runs each teardown step with a
        short timeout so the process exits promptly.

        Note: in-flight turn coroutines are interrupted by the
        cancellation that triggered this path (the ``run()``
        ``except asyncio.CancelledError`` branch).  Their queue messages
        were never acked, so on restart Pulsar redelivers them and
        a fresh turn runs from scratch -- side effects already fired
        by the original turn (Slack posts, Jira comments) may duplicate.
        That trade-off is intentional: force-stop is reserved for the
        second SIGINT, when the operator explicitly chose "exit now"
        over the (already long) graceful drain.
        """
        logger.warning("force_stop_cleanup")
        self._shutting_down = True

        async def _quiet(coro: Any) -> None:
            """Run *coro* with a 2s timeout, swallowing all errors."""
            with contextlib.suppress(Exception):
                await asyncio.wait_for(coro, timeout=2.0)

        # Same reasoning as the graceful path: force-stop still runs
        # teardown, and a watchdog exit through the middle of it turns
        # "exit now" into "exit now, and leave the sandboxes running".
        await _quiet(self._stop_loop_watchdog())
        await _quiet(self._stop_control_plane())

        # Embedded API: no graceful escalation here — force uvicorn out
        # and reap the serve task so the process can exit.
        api_server = self._api_server
        api_task = self._api_serve_task
        self._api_server = None
        self._api_serve_task = None
        if api_server is not None:
            api_server.should_exit = True
            api_server.force_exit = True
        if api_task is not None and not api_task.done():
            api_task.cancel()
            await _quiet(asyncio.gather(api_task, return_exceptions=True))

        # Reset in-memory agent state so any teardown code that still
        # inspects ``agent.state`` (extension stop hooks, dashboard
        # listeners) sees IDLE -- WORKING here would be a lie because
        # the turn coroutine was just cancelled.
        for agent in self.agent_pool.active_agents:
            if agent.state == AgentState.WORKING:
                logger.warning(
                    "force_stop_resetting_agent_state",
                    agent_handle=agent.handle,
                    task_id=agent.current_task_id,
                )
                agent.state = AgentState.IDLE
                agent.current_task_id = None

        if self.mcp_bridge:
            await _quiet(self.mcp_bridge.stop_all())
        for provider in self._llm_providers.values():
            if hasattr(provider, "close"):
                await _quiet(provider.close())
        await _quiet(self.event_queue.stop())
        self._queue_started = False
        if hasattr(self.storage, "close"):
            await _quiet(self.storage.close())

        self._running = False
        logger.warning("force_stop_complete")

    def _validate_skill_triggers(self) -> list[tuple[str, list[str], bool]]:
        """Warn when a tool skill's trigger names a tool that exists nowhere.

        Trigger matching is exact-string: a skill triggering on
        ``slack_conversations_add_message`` silently stops cataloguing —
        and, for required skills, stops *gating* — the moment an upstream
        MCP server renames the tool.  Nothing else in the engine notices,
        so this check runs whenever either side of the match can change:
        after the boot-time skill populate, after each skill webhook
        upsert, and after a live MCP-server rewire.

        Two severities, split by
        :func:`~crewlet.agent.skills.models.classify_trigger_liveness`:
        a *partially live* skill with a dangling tool leaf is almost
        certainly name drift (``warning``); a skill whose whole trigger
        matches nothing is plausibly authored for a stack this org
        doesn't run (``info``).

        Returns the findings as ``(skill_key, dangling_tools, live)``
        tuples — the log lines are the operator surface; the return
        value serves tests and introspection.
        """
        from crewlet.agent.skills.models import classify_trigger_liveness

        known_tools = {t.name for t in self.tool_registry.list_tools()}
        for role_tools in self._role_mcp_tools.values():
            known_tools.update(t.name for t in role_tools)
        known_servers = {c.name for c in self._mcp_configs}
        findings: list[tuple[str, list[str], bool]] = []
        for key in list(self._prompt_skill_registry.keys()):
            skill = self._prompt_skill_registry.get(key)
            if skill is None:
                continue
            dangling, live = classify_trigger_liveness(
                skill.trigger,
                known_tools=known_tools,
                known_servers=known_servers,
            )
            if not dangling:
                continue
            findings.append((key, dangling, live))
            if live:
                logger.warning(
                    "skill_trigger_dangling_tools",
                    skill_key=key,
                    required=skill.required,
                    dangling_tools=dangling,
                )
            else:
                logger.info(
                    "skill_trigger_inert",
                    skill_key=key,
                    dangling_tools=dangling,
                )
        return findings

    async def _start_mcp_servers(self) -> None:
        """Launch MCP servers: global (shared) + per-role instances.

        Global (``shared: true``) servers are launched once and shared
        by all agents.  ``shared: false`` servers are templates: for
        each role that declares ``role.mcp_env[name]`` a dedicated
        instance is launched with the role's overrides applied as
        environment variables (``stdio``) or HTTP headers (``http``).
        This is how each agent gets its own identity in Jira/Confluence
        (``atlassian``), Slack, GitHub, etc.

        Skipped entirely on a node that does not run seats. MCP children
        exist to serve an agent's tool calls, and a node with no agents
        makes none — so an ingress-only node launching them forks a
        subprocess tree per shared server for nothing, and does it again
        on every config activation.
        """
        from crewlet.seat.placement import NodeRole

        if not self._mcp_configs:
            return
        if not self.runs_role(NodeRole.SEATS):
            logger.info(
                "mcp_servers_not_started",
                node=self._node_id,
                count=len(self._mcp_configs),
                reason="node.roles does not include 'seats'",
            )
            return

        self.mcp_bridge = MCPToolBridge()

        # Build lookup for base configs
        cfg_by_name: dict[str, MCPServerConfig] = {c.name: c for c in self._mcp_configs}

        # 1. Launch global MCP servers (shared by all agents).  Servers
        # with ``shared: false`` are templates only — per-role instances
        # are spawned in step 2 (for both stdio and http transports).
        for cfg in self._mcp_configs:
            if not cfg.shared:
                logger.info("mcp_server_template_skipped", server=cfg.name)
                continue
            try:
                if cfg.transport == "http":
                    resolved_headers = resolve_env_vars(cfg.headers)
                    tools = await self.mcp_bridge.add_http_server(
                        name=cfg.name,
                        url=cfg.url,
                        headers=resolved_headers,
                        tool_prefix=cfg.tool_prefix,
                        annotation_overrides=cfg.annotation_overrides(),
                    )
                else:
                    resolved_env = resolve_env_vars(cfg.env)
                    _ensure_atlassian_toolsets(cfg.name, resolved_env)
                    tools = await self.mcp_bridge.add_server(
                        name=cfg.name,
                        command=cfg.command,
                        args=cfg.args,
                        env=resolved_env,
                        tool_prefix=cfg.tool_prefix,
                        annotation_overrides=cfg.annotation_overrides(),
                    )
                for tool in tools:
                    self.tool_registry.register(tool)
                logger.info(
                    "mcp_server_started",
                    server=cfg.name,
                    transport=cfg.transport,
                    tools=len(tools),
                )
            except Exception as exc:
                logger.error("mcp_server_start_failed", server=cfg.name, error=str(exc))

        # 2. Launch per-role instances from ``shared: false`` templates.
        # Each role with ``mcp_env[name]`` gets a dedicated instance
        # whose overrides carry its per-agent identity.
        for role in self.org.all_roles():
            if not role.mcp_env:
                continue
            role_tools = await self._spawn_all_role_mcp(role, cfg_by_name)
            if role_tools:
                self._role_mcp_tools[role.name] = role_tools

    async def _spawn_all_role_mcp(
        self, role: Any, cfg_by_name: dict[str, MCPServerConfig]
    ) -> list[Any]:
        """Spawn every per-role MCP instance the role declares in
        ``mcp_env``, returning the combined wrapped tools.

        Unknown server names and accidental ``shared: true`` references
        are warned (not spawned), so a misconfigured ``mcp_env`` is
        visible at spawn time rather than as an empty tool surface later.
        Shared by :meth:`_start_mcp_servers` and :meth:`_respawn_role_mcp`
        so both paths classify the same way.
        """
        role_tools: list[Any] = []
        for server_name, overrides in (role.mcp_env or {}).items():
            base_cfg = cfg_by_name.get(server_name)
            if base_cfg is None:
                logger.warning(
                    "mcp_env_unknown_server", role=role.name, server=server_name
                )
                continue
            if base_cfg.shared:
                # A shared server has one global instance; per-role
                # overrides require ``shared: false``.  Surface the
                # misconfig instead of silently double-spawning.
                logger.warning(
                    "mcp_env_for_shared_server",
                    role=role.name,
                    server=server_name,
                )
                continue
            role_tools.extend(
                await self._spawn_role_mcp_instance(
                    role, server_name, overrides, base_cfg
                )
            )
        return role_tools

    async def _spawn_role_mcp_instance(
        self,
        role: Any,
        server_name: str,
        overrides: dict[str, str],
        base_cfg: MCPServerConfig,
    ) -> list[Any]:
        """Spawn one per-role MCP instance and return its wrapped tools.

        ``overrides`` (the role's ``mcp_env[server_name]``) are layered
        over the template's base config — applied as **environment
        variables** for ``stdio`` servers and as **HTTP headers** for
        ``http`` servers (e.g. an ``Authorization`` header for a remote
        Copilot MCP).  Returns ``[]`` on failure (logged) so the caller
        can keep spawning the role's other servers.

        ``Exception`` (not ``BaseException``) is caught so a Ctrl+C
        during startup propagates as ``CancelledError`` instead of being
        swallowed mid-spawn.
        """
        if self.mcp_bridge is None:
            return []
        instance_name = mcp_instance_name(server_name, role.name)
        try:
            if base_cfg.transport == "http":
                merged_headers = {**base_cfg.headers, **overrides}
                tools = await self.mcp_bridge.add_http_server(
                    name=instance_name,
                    url=base_cfg.url,
                    headers=resolve_env_vars(merged_headers),
                    tool_prefix=base_cfg.tool_prefix,
                    annotation_overrides=base_cfg.annotation_overrides(),
                )
            else:
                merged_env = {**base_cfg.env, **overrides}
                resolved_env = resolve_env_vars(merged_env)
                _ensure_atlassian_toolsets(server_name, resolved_env)
                tools = await self.mcp_bridge.add_server(
                    name=instance_name,
                    command=base_cfg.command,
                    args=base_cfg.args,
                    env=resolved_env,
                    tool_prefix=base_cfg.tool_prefix,
                    annotation_overrides=base_cfg.annotation_overrides(),
                )
            logger.info(
                "mcp_role_server_started",
                server=instance_name,
                role=role.name,
                transport=base_cfg.transport,
                tools=len(tools),
            )
            return tools
        except Exception as exc:
            logger.error(
                "mcp_role_server_failed",
                server=instance_name,
                role=role.name,
                error=str(exc),
            )
            return []

    @property
    def is_running(self) -> bool:
        return self._running

    @property
    def started_at(self) -> str:
        """When this engine first started, ISO-8601, or ``""``.

        Stamped once at the top of :meth:`start`, before either of the
        two places ``_running`` can flip -- the unconfigured early
        return and the full startup cascade.  Keying off ``_running``
        instead would report an engine that came up unconfigured, and is
        very much running, as never started.
        """
        return self._started_at

    @property
    def shutting_down(self) -> bool:
        """True from the first moment of shutdown onwards.

        Unlike ``not is_running`` (which only flips once teardown has
        fully completed), this is already ``True`` during the graceful
        drain — it's what ``/health`` and the dashboard's footer pill
        read so operators can watch the drain live.
        """
        return self._shutting_down

    @property
    def in_flight_count(self) -> int:
        """Number of handler invocations currently mid-flight.

        Live runtime metric (not event-derived) exposed for operators
        watching a graceful shutdown drain converge to 0 -- see
        :meth:`stop` and ``docs/concepts/agent-runtime.md#graceful-shutdown``.
        """
        return getattr(self.event_queue, "in_flight_count", 0)

    # Observability hooks
    async def on_task_state_change(self, callback: _EventHandler) -> None:
        """Register a callback for any task state changes.

        Callable before or after :meth:`start`.  Before, the
        registration is held and applied when the queue comes up — a
        subscription needs a live broker connection, so the natural
        "register hooks, then start" order would otherwise raise on the
        production backend while working in tests.
        """
        group = f"hook-{id(callback)}"
        for event_type in [
            "task_created",
            "task_assigned",
            "task_started",
            "task_completed",
            "task_failed",
            "task_delegated",
        ]:
            await self._subscribe_hook(f"crewlet.events.{event_type}", group, callback)

    async def on_agent_spawn(self, callback: _EventHandler) -> None:
        """Register a callback for agent spawn events.

        Callable before or after :meth:`start` — see
        :meth:`on_task_state_change`.
        """
        group = f"hook-{id(callback)}"
        await self._subscribe_hook("crewlet.events.agent_spawned", group, callback)

    async def _subscribe_hook(
        self, topic: str, group: str, callback: _EventHandler
    ) -> None:
        """Subscribe an observability hook now, or when the queue starts."""
        if self._queue_started:
            await self.event_queue.subscribe(topic, group, callback)
            return
        self._pending_hooks.append((topic, group, callback))

    async def _apply_pending_hooks(self) -> None:
        """Attach the hooks registered before the queue was up."""
        pending, self._pending_hooks = self._pending_hooks, []
        for topic, group, callback in pending:
            await self.event_queue.subscribe(topic, group, callback)
        if pending:
            logger.debug("pending_hooks_subscribed", count=len(pending))

    # --- Runtime org mutations ---

    async def reassign(
        self,
        agent_id: str,
        new_role: str | None = None,
        new_manager: str | None = None,
    ) -> bool:
        """Reassign an agent to a different role and/or manager.

        Supports three modes:
        - ``new_role`` only: change the agent's role entirely
        - ``new_manager`` only: move the agent under a different manager
        - Both: change role and manager at the same time

        Updates definition, org hierarchy, and emits an event.
        Returns True if successful.
        """
        logger.info(
            "agent_reassigning",
            agent_id=agent_id,
            new_role=new_role or "(unchanged)",
            new_manager=new_manager or "(unchanged)",
        )
        from crewlet.agent.definition import AgentDefinition
        from crewlet.events.types import AgentReassigned
        from crewlet.org.hierarchy import get_manager
        from crewlet.org.models import RoleKind

        if new_role is None and new_manager is None:
            return False

        agent = self.agent_pool.get_by_id(agent_id)
        if agent is None:
            return False

        old_role_name = agent.role_name
        current_role = self.org.get_role(old_role_name)
        if current_role is None:
            return False

        old_manager_role = get_manager(current_role, self.org)
        old_manager_name = old_manager_role.name if old_manager_role else ""

        # Determine the target role
        if new_role is not None:
            target_role = self.org.get_role(new_role)
            if target_role is None:
                return False
            if target_role.kind != RoleKind.AGENT:
                # A human seat has no AgentInstance — reattaching a
                # live agent to it would shadow the seat with a zombie
                # runtime.  Kind flips go through apply_config.
                logger.warning(
                    "reassign_target_is_human_seat",
                    agent_id=agent_id,
                    new_role=new_role,
                )
                return False
        else:
            target_role = current_role

        # Change manager: move the role in the org hierarchy
        resolved_new_manager = ""
        if new_manager is not None:
            manager_role = self.org.get_role(new_manager)
            if manager_role is None:
                return False
            # Only remove role from old manager if no other agents
            # of the same role remain (avoids orphaning siblings)
            siblings = [
                a
                for a in self.agent_pool.get_all_for_role(current_role.name)
                if a.id_str != agent_id
            ]
            if (
                old_manager_role is not None
                and current_role.name in old_manager_role.manages
                and not siblings
            ):
                old_manager_role.manages.remove(current_role.name)
            # Add target role to new manager's manages list
            if target_role.name not in manager_role.manages:
                manager_role.manages.append(target_role.name)
            resolved_new_manager = new_manager
        else:
            new_manager_role = get_manager(target_role, self.org)
            resolved_new_manager = new_manager_role.name if new_manager_role else ""

        # Update agent definition (rebuilds system prompt with new hierarchy)
        logger.debug(
            "updating_agent_definition",
            agent_id=agent_id,
            old_role=old_role_name,
            new_role=target_role.name,
        )
        agent.definition = AgentDefinition(
            role=target_role,
            org=self.org,
        )

        await self.event_queue.publish(
            "crewlet.events.agent_reassigned",
            AgentReassigned(
                source="engine.reassign",
                agent_id=agent_id,
                old_role=old_role_name,
                new_role=target_role.name,
                old_manager=old_manager_name,
                new_manager=resolved_new_manager,
            ),
        )

        logger.info("agent_reassigned", agent_id=agent_id)
        return True

    async def _apply_restart_required_diff(self, old: Any, new: Any) -> list[str]:
        """Apply diff for subsystems that require running-process
        rewiring (MCP servers, integrations, transports, extensions,
        learning workers).

        Each branch updates the engine's stored config AND performs
        the live restart on running instances when the engine is past
        boot.  When the engine hasn't reached the spawn cascade yet
        (unconfigured-start case), only the stored config updates —
        ``_spawn_company_from_active_config`` will wire the running
        instances when the cascade runs.

        Dispatch order is ``_APPLY_DISPATCH_ORDER``.
        """
        from crewlet.engine_builders import (
            build_extensions,
            build_github_integration,
            build_gitlab_integration,
            build_notification_transports,
            build_plane_integration,
        )

        applied: list[str] = []
        # Subsystems are only live-wired once the spawn cascade has
        # populated their runtime instances.  Before then (unconfigured
        # boot, _tier_b_done False) the diff just updates the stored
        # config — the cascade reads it when it runs.
        cascade_ran = self._tier_b_done

        if old.learning != new.learning:
            applied.extend(await self._apply_learning_live(old, new))

        if old.scheduling != new.scheduling:
            applied.extend(await self._apply_scheduling_live(old, new))

        if old.mcp_servers != new.mcp_servers:
            new_mcp_configs = parse_mcp_servers(new.mcp_servers)
            old_mcp_configs = list(self._mcp_configs)
            self._mcp_configs = new_mcp_configs
            # The turn engine captured the old list by reference; ``_mcp_configs``
            # is reassigned (not mutated in place), so refresh the sandbox
            # backend's view too — its scoped MCP rendering reads this.
            if self.turn_engine is not None:
                self.turn_engine.set_sandbox_mcp_servers(new_mcp_configs)
            if cascade_ran:
                await self._apply_mcp_servers_live(old_mcp_configs, new_mcp_configs)
                # Restarted servers can rename / drop tools — re-check
                # every skill trigger against the rewired surface.
                self._validate_skill_triggers()
            applied.append("mcp_servers")

        old_int = old.integrations
        new_int = new.integrations

        atlassian_changed = (
            old_int.jira != new_int.jira or old_int.confluence != new_int.confluence
        )
        if atlassian_changed:
            # jira + confluence are non-tool config now (admin REST +
            # webhooks); the atlassian MCP *tool* server lives in
            # ``mcp_servers`` (handled by the branch above) and the
            # org-wide search spaces live in ``knowledge.confluence_spaces``
            # (handled by the org diff).  Here we only rebuild the
            # Jira/Confluence transports and refresh the Atlassian
            # account→handle routing map.
            new_transports = (
                build_notification_transports(new, storage=self.storage)
                + self._custom_transports
            )
            self._pending_transports = new_transports
            if cascade_ran:
                await self._apply_notification_transports_live(new_transports)
                await self._refresh_atlassian_handles(new)
            applied.append("integrations_atlassian")

        if old_int.slack != new_int.slack:
            # Slack is a transport-enable marker now (the Slack MCP tool
            # server lives in ``mcp_servers``).  Toggling it adds/removes
            # the SlackTransport, which re-seeds per-agent apps from the
            # org inside ``_apply_notification_transports_live``.
            new_transports = (
                build_notification_transports(new, storage=self.storage)
                + self._custom_transports
            )
            self._pending_transports = new_transports
            if cascade_ran:
                await self._apply_notification_transports_live(new_transports)
            applied.append("integrations_slack")

        if old_int.github != new_int.github:
            self._github_config = build_github_integration(new, self.org)
            if cascade_ran:
                await self._refresh_github_handles(new)
            applied.append("integrations_github")

        if old_int.gitlab != new_int.gitlab:
            self._gitlab_config = build_gitlab_integration(new, self.org)
            if self.notification_service is not None:
                self.notification_service.set_gitlab_config(self._gitlab_config)
            if cascade_ran:
                await self._refresh_gitlab_handles(new)
            applied.append("integrations_gitlab")

        if old_int.plane != new_int.plane:
            # Plane has BOTH a transport (webhook routing, like
            # jira/confluence) and a per-agent identity registry (like
            # github/gitlab): rebuild the transport set AND refresh the
            # Plane user-UUID→handle routing map.  The PlaneTransport
            # carries its own resolved config (Confluence precedent), so
            # there is no service-side config to push.
            self._plane_config = build_plane_integration(new, self.org)
            new_transports = (
                build_notification_transports(new, storage=self.storage)
                + self._custom_transports
            )
            self._pending_transports = new_transports
            if cascade_ran:
                await self._apply_notification_transports_live(new_transports)
                await self._refresh_plane_handles(new)
            applied.append("integrations_plane")

        if old.extensions != new.extensions:
            new_extensions = build_extensions(new)
            self._pending_extensions = new_extensions
            if cascade_ran:
                await self._apply_extensions_live(
                    new_extensions,
                    old_cfg_entries=old.extensions,
                    new_cfg_entries=new.extensions,
                )
            applied.append("extensions")

        return applied

    def _seed_seat_budget(self, role: Any, org: Any) -> None:
        """Apply ``role``'s ``token_budget`` cap to the manager.

        Keyed on the seat's DERIVED id, and seeded for every agent seat
        in the org — not only the ones spawned in this process.  Caps
        are config; only *usage* is shared (``PostgresBudgetUsageStore``),
        and ``BudgetManager.spend`` passes ``agent_limit=None`` when it
        has no local cap for an id, which the store reads as unlimited.
        So a node that had not seeded a seat would run that seat's first
        turn after a takeover with no per-agent cap at all — the one
        failure mode a budget must not have.  Seeding from the org costs
        one int per seat and removes the question.

        A 0 budget means "unlimited" — skip the call so we don't seed a
        0 cap that the budget manager would treat as immediately
        exhausted.
        """
        if role.is_human or role.token_budget <= 0:
            return
        agent_id = org.agent_id_for(role)
        if not agent_id:
            return
        self.budget_manager.set_agent_budget(agent_id, role.token_budget)
        logger.debug(
            "agent_token_budget_set",
            agent_id=agent_id,
            role=role.name,
            budget=role.token_budget,
        )

    def _reseed_seat_budgets(self, org: Any) -> None:
        """Make the per-seat token caps a projection of ``org``.

        Caps are config, so they are derived from the active revision
        rather than accumulated: every agent seat with a positive
        ``token_budget`` gets its cap (usage history preserved via
        ``update_agent_budget``), and every cap whose seat is gone —
        role removed, flipped to human, budget dropped to 0
        ("unlimited") — is dropped.

        Called on every org swap, including the rollback path.  A
        rollback that had already applied a per-role budget change used
        to leave the *failed* revision's caps in place, because only the
        org-level cap was in the snapshot.
        """
        wanted: dict[str, int] = {}
        for role in org.all_roles():
            if role.is_human or role.token_budget <= 0:
                continue
            agent_id = org.agent_id_for(role)
            if agent_id:
                wanted[agent_id] = role.token_budget
        for agent_id, cap in wanted.items():
            self.budget_manager.update_agent_budget(agent_id, cap)
        for agent_id in self.budget_manager.agent_budget_ids():
            if agent_id not in wanted:
                self.budget_manager.drop_agent_budget(agent_id)

    def _apply_restart_required_subsystem(
        self,
        attr_name: str,
        new_value: Any,
        subsystem: str,
        hint: str,
    ) -> list[str]:
        """Store a new config block for a subsystem that can't live-rewire.

        Honest contract for subsystems whose runtime workers (learning's
        ReflectEngine / EpisodeLifecycleWorker / SkillCuratorWorker,
        scheduling's Scheduler) wire deeply at boot — storage,
        embeddings, tool registry, event-queue subscriptions captured
        by reference.  Re-creating them post-boot would need re-running
        a ~600-line block of ``start()`` with the new config; rather
        than half-rewire (stopping workers and silently leaving them
        stopped), we store the new config so the next engine
        restart picks it up and log a loud WARNING that the running
        workers continue on the prior settings until then.

        Operators see the change reflected in ``GET /config`` and the
        dashboard revision history immediately; the worker behaviour
        only changes after a restart.  That contract beats silent
        degradation.
        """
        setattr(self, attr_name, new_value)
        if not self._tier_b_done:
            logger.info(f"{subsystem}_config_updated_pre_cascade")
            return [subsystem]
        logger.warning(f"{subsystem}_config_restart_required", hint=hint)
        return [subsystem]

    async def _apply_learning_live(self, old: Any, new: Any) -> list[str]:
        return self._apply_restart_required_subsystem(
            "_learning_config",
            new.learning,
            "learning",
            hint=(
                "learning: settings have changed.  The running "
                "ReflectEngine / EpisodeLifecycleWorker / "
                "SkillCuratorWorker continue on the prior config; "
                "restart the engine for the new settings to take "
                "effect.  See docs/concepts/configuration.md."
            ),
        )

    async def _apply_scheduling_live(self, old: Any, new: Any) -> list[str]:
        return self._apply_restart_required_subsystem(
            "_scheduling_config",
            new.scheduling,
            "scheduling",
            hint=(
                "scheduling: settings have changed.  The running "
                "Scheduler continues on the prior tick/jitter/catchup/"
                "timezone/enabled values; restart the engine for the "
                "new settings to take effect.  Role/unit schedule list "
                "changes still apply live."
            ),
        )

    async def _drain_seat(self, role_name: str) -> bool:
        """Wait for one seat's in-flight turns before mutating it.

        A turn already pins the config it started under
        (:mod:`crewlet.agent.turn_pin`), so it stays internally coherent
        through a live apply.  But a pin holds a *catalogue*, not a
        *capability*: the MCP clients its tools dispatch to can still be
        stopped underneath it, and the definition it was spawned from can
        be replaced. Draining is what turns "the turn survives the
        rewire" into "the rewire happens between turns".

        No-op before the turn engine exists (first activation, per-entity
        bootstrap). Returns whether the seat reached idle.
        """
        if self.turn_engine is None:
            return True
        return await self.turn_engine.drain_seat(role_name)

    async def _stop_role_mcp(self, role: Any) -> None:
        """Stop every per-role MCP instance for ``role``.

        Covers every ``shared: false`` template in ``self._mcp_configs``
        (stdio and http alike — atlassian, slack, github, …).  Shared by
        :meth:`_respawn_role_mcp` (which then re-spawns) and the
        removed-role branch of :meth:`_apply_org_diff` (which tears the
        role down for good).  Does NOT pop ``self._role_mcp_tools`` —
        respawn rebuilds that map, the removal path pops it explicitly.
        No-op when no bridge exists.
        """
        if self.mcp_bridge is None:
            return
        for cfg in self._mcp_configs:
            if cfg.shared:
                continue
            instance = mcp_instance_name(cfg.name, role.name)
            try:
                await self.mcp_bridge.stop_server(instance)
            except Exception as exc:
                logger.warning(
                    "mcp_role_server_stop_failed",
                    server=instance,
                    role=role.name,
                    error=str(exc),
                )

    async def _respawn_role_mcp(self, role: Any) -> None:
        """Stop and re-spawn every per-role MCP instance for ``role``.

        Used after a role's ``mcp_env`` changes (or a per-agent token
        rotates) — the per-role MCP processes baked in the prior
        credentials and need restarting to see the new values.

        Walks every ``shared: false`` template in ``self._mcp_configs``
        and re-spawns the ones the role still has overrides for (stdio
        env or http headers, via :meth:`_spawn_role_mcp_instance`).  The
        role's tool cache in ``self._role_mcp_tools`` is rebuilt from the
        survivors.
        """
        # The bridge may not exist yet on a per-entity bootstrap that
        # adds roles before any ``shared: true`` MCP server -- spawn it
        # on demand so the per-role templates the new role declares land.
        if self.mcp_bridge is None:
            if not role.mcp_env:
                return
            self.mcp_bridge = MCPToolBridge()
        # This is the mutation the turn pin CANNOT survive: the pin holds
        # the tool wrappers, but the clients they dispatch to are about
        # to be killed.  Drain the seat first, here rather than at each
        # call site, so every caller (org diff, credential rotation, the
        # rotation-only apply path) gets it.  Bounded — see
        # ``SEAT_DRAIN_TIMEOUT_SECONDS``.
        await self._drain_seat(role.name)
        # Tear down the role's existing per-role instances.
        cfg_by_name: dict[str, MCPServerConfig] = {c.name: c for c in self._mcp_configs}
        await self._stop_role_mcp(role)

        # Re-spawn each per-role server the role still has overrides for.
        role_tools = await self._spawn_all_role_mcp(role, cfg_by_name)

        if role_tools:
            self._role_mcp_tools[role.name] = role_tools
        else:
            # Nothing landed for this role.  Make it loud at WARNING so
            # the user sees the "agent has no per-role tools" condition
            # immediately instead of through the downstream
            # ``list_mcp_server_tools(server=...) returned (none)``
            # symptom at turn time.
            self._role_mcp_tools.pop(role.name, None)
            logger.warning(
                "role_has_no_per_role_mcp_tools",
                role=role.name,
                has_mcp_env=bool(role.mcp_env),
                hint=(
                    "no per-role MCP servers spawned -- check that the "
                    "``${VAR}`` references in role.mcp_env resolve in the "
                    "engine's environment, the named servers exist in "
                    "mcp_servers with ``shared: false``, or this is a "
                    "role with no per-agent integrations"
                ),
            )

    async def _apply_credential_rotation(self, cfg: Any) -> list[str]:
        """Rebuild every subsystem that CAPTURED a resolved credential.

        Reached when the config payload is byte-identical but its
        ``${VAR}`` references now resolve differently — i.e. an operator
        rotated a secret and re-activated the revision to pick it up.

        The ordinary diff handlers cannot do this: every one of them
        compares raw config (``old.providers.llm != new.providers.llm``,
        ``old.mcp_servers != new.mcp_servers``, the role signature behind
        ``_respawn_role_mcp``), and on this path all of those are equal by
        construction. So the rebuild is unconditional for the subsystems
        that hold a resolved value, and skipped entirely for those that
        do not.

        Best-effort per subsystem: a rotation that cannot rebuild one
        transport must still reach the others, and must not roll the
        engine back to a revision it is already running.
        """
        from crewlet.engine_builders import (
            build_llm_providers,
            build_notification_transports,
        )

        applied: list[str] = []

        # LLM providers hold the key inside a constructed client.
        try:
            providers, provider_configs = build_llm_providers(cfg)
            if providers:
                self._llm_providers.clear()
                self._llm_providers.update(providers)
                self._llm_provider_configs.clear()
                self._llm_provider_configs.update(provider_configs)
                applied.append("providers")
        except Exception as exc:
            logger.error("rotation_providers_failed", error=str(exc))

        # MCP children baked the resolved env into their spawn
        # environment, so nothing short of a restart picks up a new one.
        try:
            configs = parse_mcp_servers(cfg.mcp_servers)
            for name in list(configs):
                await self._mcp_bridge.restart_server(name)
            if configs:
                applied.append("mcp_servers")
        except Exception as exc:
            logger.error("rotation_mcp_failed", error=str(exc))

        # Per-role MCP children hold that seat's own credentials.
        try:
            respawned = 0
            for agent in list(self.agent_pool.agents):
                role = getattr(agent.definition, "role", None)
                if role is not None and getattr(role, "mcp_env", None):
                    await self._respawn_role_mcp(role)
                    respawned += 1
            if respawned:
                applied.append("role_mcp")
        except Exception as exc:
            logger.error("rotation_role_mcp_failed", error=str(exc))

        # Transports hold tokens in clients and headers.
        try:
            new_transports = (
                build_notification_transports(cfg, storage=self.storage)
                + self._custom_transports
            )
            if new_transports:
                await self._apply_notification_transports_live(new_transports)
                applied.append("transports")
        except Exception as exc:
            logger.error("rotation_transports_failed", error=str(exc))

        logger.info("credential_rotation_applied", subsystems=applied)
        return applied

    async def _apply_mcp_servers_live(
        self,
        old_configs: list[Any],
        new_configs: list[Any],
    ) -> None:
        """Add / remove / restart MCP server processes to match the
        new config list."""
        # Per-entity bootstrap can introduce the very first MCP server
        # via ``POST /config/mcp-servers`` long after engine start, by
        # which point ``_start_mcp_servers`` has already early-returned
        # (no configs at first-activation) and ``self.mcp_bridge`` is
        # ``None``.  Lazy-create the bridge so live additions land.
        if self.mcp_bridge is None:
            if not new_configs:
                return
            self.mcp_bridge = MCPToolBridge()
        old_by_name = {c.name: c for c in old_configs}
        new_by_name = {c.name: c for c in new_configs}

        # Removed: stop the running client + drop its tools.
        for name in old_by_name.keys() - new_by_name.keys():
            # Capture the tool names BEFORE stopping — the bridge drops
            # its own index inside stop_server, and until now nothing
            # dropped them from the shared ToolRegistry. A server removed
            # by a live config edit therefore kept its tools in every
            # later turn's catalogue, dispatching to a stopped client
            # forever: a soft `success=False` the model burns rounds
            # retrying, with nothing in the logs to explain it.
            doomed = [t.name for t in self.mcp_bridge.get_server_tools(name)]
            try:
                await self.mcp_bridge.stop_server(name)
                for tool_name in doomed:
                    self.tool_registry.unregister(tool_name)
                logger.info(
                    "mcp_server_stopped_live", server=name, tools_dropped=len(doomed)
                )
            except Exception as exc:
                logger.warning("mcp_server_stop_failed", server=name, error=str(exc))

        # Added or changed: start (or restart) with the new spec.
        for name, cfg in new_by_name.items():
            if not getattr(cfg, "shared", True):
                # Per-role servers are spawned in the cascade, not
                # the global bridge; skip here.
                continue
            old_cfg = old_by_name.get(name)
            # Cover BOTH transports: an http server's identity is its
            # url/headers/tool_prefix, a stdio server's its
            # command/args/env/tool_prefix.  The prior check only looked
            # at the stdio fields, so a live edit to a shared http MCP's
            # url or headers (e.g. rotating a remote token) was a silent
            # no-op — the stale connection kept serving.
            needs_restart = (
                old_cfg is None
                or old_cfg.transport != cfg.transport
                or old_cfg.command != cfg.command
                or old_cfg.args != cfg.args
                or old_cfg.env != cfg.env
                or old_cfg.url != cfg.url
                or old_cfg.headers != cfg.headers
                or old_cfg.tool_prefix != cfg.tool_prefix
            )
            if not needs_restart:
                continue
            try:
                if cfg.transport == "http":
                    # ``restart_server`` would relaunch this as a stdio
                    # subprocess with an empty command; use the http
                    # path so the remote connection is re-established.
                    wrapped = await self.mcp_bridge.restart_http_server(
                        name=name,
                        url=cfg.url,
                        headers=resolve_env_vars(cfg.headers),
                        tool_prefix=cfg.tool_prefix,
                        annotation_overrides=cfg.annotation_overrides(),
                    )
                else:
                    # Resolve ``${VAR}`` env references the same way
                    # ``_start_mcp_servers`` does — the DB payload keeps
                    # them verbatim, so passing ``cfg.env`` raw would
                    # hand the subprocess the literal ``${...}`` string.
                    resolved_env = resolve_env_vars(cfg.env)
                    _ensure_atlassian_toolsets(name, resolved_env)
                    wrapped = await self.mcp_bridge.restart_server(
                        name=name,
                        command=cfg.command,
                        args=cfg.args,
                        env=resolved_env,
                        tool_prefix=cfg.tool_prefix,
                        annotation_overrides=cfg.annotation_overrides(),
                    )
                # Re-register the new wrapped tools with the engine
                # tool registry so subsequent turns see them.
                for tool in wrapped:
                    self.tool_registry.register(tool)
                logger.info(
                    "mcp_server_restarted_live",
                    server=name,
                    transport=cfg.transport,
                    tool_count=len(wrapped),
                )
            except Exception as exc:
                logger.error("mcp_server_restart_failed", server=name, error=str(exc))

    async def _apply_notification_transports_live(
        self, new_transports: list[Any]
    ) -> None:
        """Swap NotificationService.transports dict to the new list.

        ``NotificationService.start()`` only runs once at engine boot,
        so transports added later via live config have to be ``start()``
        ed explicitly here -- without it the SlackTransport (and any
        other transport) sits with ``_running=False`` and rejects every
        inbound webhook with ``handle_event_after_stop``.  Symmetric
        ``stop()`` on the outgoing set drains its in-flight handlers
        and releases any open connections.
        """
        if self.notification_service is None:
            return
        old_dict: dict[str, Any] = dict(self.notification_service.transports)
        new_dict: dict[str, Any] = {t.name: t for t in new_transports}

        # Stop transports the swap evicts (removed entirely or replaced
        # by a fresh instance with the same name).
        for name, old_transport in old_dict.items():
            if new_dict.get(name) is old_transport:
                continue
            try:
                await old_transport.stop()
            except Exception as exc:
                logger.warning(
                    "transport_stop_failed_live", transport=name, error=str(exc)
                )

        self.notification_service.transports = new_dict
        self._share_delivery_dedupe(new_dict)

        # Re-seed routing state BEFORE starting anything: it is an INPUT
        # to ``start()``, not something a started transport acquires
        # later.  A fresh transport instance begins with empty routing
        # state (per-agent Slack apps, project/space key→lead maps,
        # handle registry), and for Mattermost that state is what
        # ``start()`` consumes — it resolves each bot's identity and
        # opens the websocket fleet, and the fleet refuses to start
        # without the event queue.  Started first, Mattermost would log
        # one ``mattermost_fleet_not_started_no_queue`` and go
        # permanently deaf: nothing starts a transport twice.  The
        # webhook-driven transports need the same ordering for a less
        # dramatic reason — Slack would answer ``no_app_for_handle`` and
        # Jira/Confluence/Plane fall-through routing would misroute
        # until the engine restarted.
        if "slack" in new_dict and old_dict.get("slack") is not new_dict["slack"]:
            self._refresh_slack_apps()
        if (
            "mattermost" in new_dict
            and old_dict.get("mattermost") is not new_dict["mattermost"]
        ):
            self._refresh_mattermost_bots()
        self._reseed_notification_routing()

        # Start the newly-installed transports (replacements + brand
        # new ones).  ``NotificationService`` already subscribed at
        # service.start(); new transports just need their own start().
        for name, new_transport in new_dict.items():
            if old_dict.get(name) is new_transport:
                continue
            try:
                await new_transport.start()
            except Exception as exc:
                logger.error(
                    "transport_start_failed_live", transport=name, error=str(exc)
                )

        # The per-transport refreshers above only run for transports
        # PRESENT in the new dict — when the knowledge backend was
        # REMOVED outright, this is the only hook that can null the
        # searcher / worker and re-point the TurnEngine (M3: swap AND
        # removal).  Idempotent for every other case.
        self._refresh_knowledge_backend()

        logger.info(
            "notification_transports_swapped_live",
            transports=sorted(new_dict.keys()),
        )

    def _reseed_notification_routing(self) -> None:
        """Re-seed routing state (handle registry, project/space→lead
        maps, tool-skill container exclusions) on the currently
        installed Jira / Confluence / Plane transports.

        Two callers: :meth:`_apply_notification_transports_live` (a
        freshly rebuilt transport starts with empty routing state) and
        :meth:`_apply_org_diff` after the org swap (an org-only edit —
        a unit's ``integrations.*.project`` identity or its lead —
        changes the lead maps while the transport instances survive;
        without this call the running transports keep routing on the
        stale maps until a restart).  Each refresher is idempotent and
        no-ops on a missing or foreign-typed transport.
        """
        if self.notification_service is None:
            return
        transports = self.notification_service.transports
        if "jira" in transports:
            self._refresh_jira_routing(transports["jira"])
        if "confluence" in transports:
            self._refresh_confluence_routing(transports["confluence"])
        if "plane" in transports:
            self._refresh_plane_routing(transports["plane"])

    async def _refresh_atlassian_handles(self, new: Any) -> None:
        """Re-register Jira account handles from the org against the
        running handle registry."""
        if self.handle_registry is None or self.mcp_bridge is None:
            return
        if new.integrations.jira is None:
            return
        try:
            await register_jira_accounts_from_org(
                self.handle_registry,
                self.org,
                mcp_bridge=self.mcp_bridge,
            )
            logger.info("jira_handles_refreshed_live")
        except Exception as exc:
            logger.error("jira_handle_refresh_failed", error=str(exc))

    def _share_delivery_dedupe(self, transports: Any) -> None:
        """Point every transport's delivery dedupe at the shared store.

        Each transport derives its own dedupe key — what counts as "the
        same delivery" is genuinely source-specific — but the STORE has
        to be shared, or a provider retry that lands on a peer node is a
        fresh delivery there and the agent answers twice.

        No database means no peers to share with, so the per-process
        default stands.
        """
        from crewlet.db.client import Database
        from crewlet.db.deliveries import PostgresDeliveryDedupeStore

        if not isinstance(self.storage, Database):
            return
        store = PostgresDeliveryDedupeStore(self.storage)
        for transport in (transports or {}).values():
            setter = getattr(transport, "set_delivery_dedupe", None)
            if setter is not None:
                setter(store)

        # The notification valve shares the same reasoning: per-process
        # counters multiply the limit by replica count and cannot see a
        # loop that bounces between nodes.
        service = getattr(self, "notification_service", None)
        if service is not None:
            from crewlet.db.rate_limits import PostgresRateLimitStore

            service.set_rate_limit_store(PostgresRateLimitStore(self.storage))

    def _start_maintenance_worker(self) -> None:
        """Build the retention sweep, if there is anything to sweep.

        Skipped without a database, and correctly so: the memory twins
        of these stores prune themselves inline, because a process-local
        dict dies with the process. Only the Postgres tables accumulate,
        and only they need a sweeper.

        Behind ``worker:maintenance`` rather than per node: the deletes
        are idempotent range deletes, so N nodes running them would not
        corrupt anything — it would simply be N times the write
        amplification and vacuum churn for one table's worth of benefit.
        """
        from crewlet.db.client import Database
        from crewlet.db.deliveries import PostgresDeliveryDedupeStore
        from crewlet.db.maintenance import MaintenanceWorker
        from crewlet.db.rate_limits import PostgresRateLimitStore
        from crewlet.schedule import ScheduledRunStore

        if self._maintenance_worker is not None:
            return
        if not isinstance(self.storage, Database):
            return
        a2a_channels = (
            self.a2a_service.channels if self.a2a_service is not None else None
        )
        self._maintenance_worker = MaintenanceWorker(
            deliveries=PostgresDeliveryDedupeStore(self.storage),
            rate_limits=PostgresRateLimitStore(self.storage),
            scheduled_runs=ScheduledRunStore(self.storage),
            turn_completions=self._turn_completions,
            a2a_channels=a2a_channels,
            apply_status=self._config_plane(),
            # Closing a channel nothing finished is housekeeping on the
            # same shared table, on the same singleton, for the same
            # reason: N nodes closing the same abandoned channels would
            # publish N closed events for each one.
            close_idle_a2a=(
                self.a2a_service.close_idle_channels
                if self.a2a_service is not None
                else None
            ),
            claim_duty=lambda: self.claim_worker_duty("maintenance"),
        )

    def _build_a2a_channel_store(self) -> None:
        """Point the A2A service at durable channel state, if there is any.

        Without a database the service keeps its in-memory twin, which
        is process-local — so cross-node authorization does not work,
        exactly as seat placement does not. Same rule, said once here
        and loudly at boot there.
        """
        from crewlet.a2a.channels import PostgresA2AChannelStore
        from crewlet.db.client import Database

        if self.a2a_service is None or not isinstance(self.storage, Database):
            return
        self.a2a_service.set_channel_store(PostgresA2AChannelStore(self.storage))

    def _build_turn_completion_store(self) -> None:
        """Wire the completion ledger, if there is somewhere to keep it.

        Postgres or nothing. The memory twin exists and is used by tests,
        but wiring it by default here would be worse than no ledger: it
        is process-local, so it cannot deduplicate across the takeover
        that is the entire reason the table exists, while looking from
        the outside exactly as though it does.
        """
        from crewlet.db.client import Database
        from crewlet.db.turn_completions import PostgresTurnCompletionStore

        if self._turn_completions is not None:
            return
        if not isinstance(self.storage, Database):
            return
        self._turn_completions = PostgresTurnCompletionStore(self.storage)

    def _refresh_jira_routing(self, jira_transport: Any) -> None:
        """Re-seed a freshly-rebuilt JiraTransport's routing state.

        A new transport instance starts with an empty handle registry
        and an empty project-key→lead map, so the webhook fall-through
        path (assign an unrouted issue to its project's unit lead) would
        misroute or drop until restart.  Mirrors the boot wiring in
        ``start()``.  ``register_jira_accounts_from_org`` (async, MCP) is
        run separately by ``_refresh_atlassian_handles``.
        """
        from crewlet.config import build_project_key_lead_map
        from crewlet.notifications.transports.jira import JiraTransport

        if not isinstance(jira_transport, JiraTransport):
            return
        if self.handle_registry is not None:
            jira_transport.set_handle_registry(self.handle_registry)
        # Always push the freshly built map — the setter replaces the
        # dict wholesale, so an EMPTY map is the only way to CLEAR live
        # routing when the last ``integrations.jira.project`` identity
        # (or its lead) is removed from the org.  The setter's own
        # ``if mapping:`` guard keeps the empty case quiet.
        jira_transport.set_project_key_leads(build_project_key_lead_map(self.org))
        logger.info("jira_routing_refreshed_live")

    # ── knowledge backend wiring (searcher + Tool Skills sync) ──────

    def _make_page_event_callback(self) -> Any:
        """Build the index-callback closure both backends register.

        Forwards a page webhook (``(event_type, page_id)``) to the
        CURRENT sync worker — read per event, not captured, so a
        rebuilt worker takes over without re-registering — then
        re-checks skill triggers against the live tool surface (a
        skill edit can introduce or fix a trigger tool name).
        """

        async def _on_page_event(event_type: str, page_id: str) -> None:
            et = (event_type or "").lower()
            ts_worker = self._tool_skill_sync_worker
            if ts_worker is not None:
                await ts_worker.handle_page_event(page_id=page_id, event_kind=et)
                self._validate_skill_triggers()

        return _on_page_event

    def _wire_confluence_skill_sync(self, confluence_transport: Any) -> None:
        """Construct the Confluence Tool Skills sync worker and register
        the index callback on ``confluence_transport``.

        Shared by the ``start()`` cascade and the live-refresh
        reconcile (:meth:`_refresh_knowledge_backend`) so a rebuilt
        transport never orphans the sync (worker holding the stopped
        transport's client, new transport with no callback).  An empty
        ``CREWLET_TOOL_SKILLS_SPACE`` disables the sync entirely.
        """
        if not getattr(self, "_tool_skill_space_key", ""):
            return
        from crewlet.agent.skills import ToolSkillSyncWorker

        self._tool_skill_sync_worker = ToolSkillSyncWorker(
            transport=confluence_transport,
            registry=self._prompt_skill_registry,
            space_key=self._tool_skill_space_key,
        )
        confluence_transport.set_index_callback(self._make_page_event_callback())

    def _wire_plane_skill_sync(self, plane_transport: Any) -> None:
        """Construct the Plane Tool Skills sync worker and register the
        index callback on ``plane_transport``.

        The Plane counterpart of :meth:`_wire_confluence_skill_sync`,
        with the same two callers.  An empty
        ``CREWLET_TOOL_SKILLS_PROJECT`` disables the sync entirely.
        """
        if not getattr(self, "_tool_skill_project_key", ""):
            return
        from crewlet.agent.skills import PlaneSkillSyncWorker

        self._tool_skill_sync_worker = PlaneSkillSyncWorker(
            transport=plane_transport,
            registry=self._prompt_skill_registry,
            project=self._tool_skill_project_key,
        )
        plane_transport.set_index_callback(self._make_page_event_callback())

    def _kick_tool_skill_resync(self) -> None:
        """Kick a full registry populate off the current sync worker,
        with a bounded retry, then re-validate skill triggers.

        Used by the ``start()`` boot walk and by the live-refresh
        reconcile — after a transport rebuild or a backend cut-over the
        registry must be re-seeded from the new backend, or it would
        keep serving the old backend's skills indefinitely.  No worker
        ⇒ no-op.

        A walk that FAILS (``run_initial_sync() -> None``: backend
        unreachable, project/space unresolved, incomplete enumeration)
        retries up to ``_TOOL_SKILL_RESYNC_ATTEMPTS`` times with
        exponential backoff — sized for the compose boot race where the
        backend comes up seconds after the engine (see the constants'
        comment).  When every attempt fails the walk gives up LOUDLY
        (``tool_skill_resync_exhausted``): the registry keeps whatever
        it currently holds — on a cut-over that is the old backend's
        skills, which beats an empty prompt surface — and the operator
        is told to fix the backend and re-apply the integration (or
        restart) to re-walk.  A re-kick cancels a still-retrying older
        walk so two walks never race one registry.
        """
        if self._tool_skill_sync_worker is None:
            return
        previous = self._tool_skill_resync_task
        if previous is not None and not previous.done():
            previous.cancel()

        worker = self._tool_skill_sync_worker

        async def _run_tool_skill_walk() -> None:
            for attempt in range(1, _TOOL_SKILL_RESYNC_ATTEMPTS + 1):
                loaded: int | None
                try:
                    loaded = await worker.run_initial_sync()
                except Exception:
                    # The workers never raise by contract; belt and
                    # braces so a bug can't kill the retry loop.
                    logger.exception("tool_skill_initial_sync_failed")
                    loaded = None
                if loaded is not None:
                    self._validate_skill_triggers()
                    return
                if attempt < _TOOL_SKILL_RESYNC_ATTEMPTS:
                    delay = _TOOL_SKILL_RESYNC_BASE_DELAY_SECONDS * (2 ** (attempt - 1))
                    logger.warning(
                        "tool_skill_resync_retry",
                        attempt=attempt,
                        max_attempts=_TOOL_SKILL_RESYNC_ATTEMPTS,
                        retry_in_seconds=delay,
                    )
                    await asyncio.sleep(delay)
            logger.error(
                "tool_skill_resync_exhausted",
                attempts=_TOOL_SKILL_RESYNC_ATTEMPTS,
                registry_keys=len(self._prompt_skill_registry),
                hint="the Tool Skills registry keeps its previous contents "
                "(possibly a decommissioned backend's skills after a "
                "cut-over); fix the knowledge backend, then re-apply the "
                "integrations config or restart the engine to re-walk",
            )

        self._tool_skill_resync_task = asyncio.create_task(_run_tool_skill_walk())

    @staticmethod
    def _select_knowledge_backend(confluence: Any, plane: Any) -> tuple[str, Any]:
        """The ONE place that decides which knowledge backend is active.

        Returns ``("confluence"|"plane"|"none", transport_or_None)``.
        Every consumer of the decision — the ``start()`` wiring, the
        live-refresh reconcile, and the promotion-writer gate — routes
        through here so they can never disagree on the tiebreak.
        Config validation (confluence × plane exclusivity,
        ``config.py``) makes the both-present case unreachable; the
        Confluence-first order is a defensive tiebreak only, chosen to
        match the historical ``start()`` behaviour.
        """
        if confluence is not None:
            return "confluence", confluence
        if plane is not None:
            return "plane", plane
        return "none", None

    def _install_knowledge_backend(self) -> str:
        """Build the searcher + Tool Skills sync worker for the selected
        backend off ``_confluence_transport`` / ``_plane_transport``.

        Shared by the ``start()`` cascade and
        :meth:`_refresh_knowledge_backend` so both derive the exact
        same machinery from the same selection
        (:meth:`_select_knowledge_backend`).  Returns the backend name.
        Neither transport ⇒ searcher ``None`` + no worker (the Plan
        prefetch renders nothing, the skills subsystem is inert).
        """
        backend, _transport = self._select_knowledge_backend(
            getattr(self, "_confluence_transport", None),
            getattr(self, "_plane_transport", None),
        )
        self._tool_skill_sync_worker = None
        searcher: Any = None
        if backend == "confluence":
            from crewlet.knowledge.confluence_search import ConfluenceSearcher

            searcher = ConfluenceSearcher(transport=self._confluence_transport)
            self._wire_confluence_skill_sync(self._confluence_transport)
            logger.info(
                "confluence_search_wired",
                tool_skills_space=getattr(self, "_tool_skill_space_key", "")
                or "(disabled)",
            )
        elif backend == "plane":
            from crewlet.knowledge.plane_search import PlaneSearcher

            searcher = PlaneSearcher(
                transport=self._plane_transport,
                skills_project=getattr(self, "_tool_skill_project_key", ""),
            )
            self._wire_plane_skill_sync(self._plane_transport)
            logger.info(
                "plane_knowledge_wired",
                tool_skills_project=getattr(self, "_tool_skill_project_key", "")
                or "(disabled)",
            )
        self._knowledge_searcher = searcher
        return backend

    def _build_promotion_page_writer(self) -> Any:
        """Pick the promotion page writer for the active knowledge
        backend, or ``None`` when neither backend is wired (the
        promotion pass then soft-no-ops).  The backend decision —
        including the defensive both-present tiebreak — lives in
        :meth:`_select_knowledge_backend`, shared with the searcher /
        sync-worker wiring, so promotion can never write to a different
        backend than the one searching and syncing.
        """
        backend, transport = self._select_knowledge_backend(
            getattr(self, "_confluence_transport", None),
            getattr(self, "_plane_transport", None),
        )
        if backend == "confluence":
            from crewlet.confluence.promotion import ConfluencePromotionWriter

            return ConfluencePromotionWriter(transport=transport)
        if backend == "plane":
            from crewlet.plane.promotion import PlanePromotionWriter

            return PlanePromotionWriter(transport=transport)
        return None

    def _refresh_knowledge_backend(self) -> None:
        """Reconcile the knowledge machinery with the installed
        transports after a live change.

        The searcher, the Tool Skills sync worker, and the index
        callback all hold the transport they were built against, and
        the TurnEngine captures the searcher reference at
        construction — so a live ``integrations`` PUT that rebuilds a
        transport (or cuts over between backends, or removes the
        integration) must rebuild all three and re-point the
        TurnEngine via ``set_knowledge_searcher`` (swap AND removal),
        then re-seed the registry from the new backend.  Identity
        check first: an org-only edit (transports unchanged) is a
        no-op — the search scope reads ``org`` per call and needs no
        refresh hook.
        """
        from crewlet.notifications.transports.confluence import ConfluenceTransport
        from crewlet.notifications.transports.plane import PlaneTransport

        transports: dict[str, Any] = (
            dict(self.notification_service.transports)
            if self.notification_service is not None
            else {}
        )
        confluence = transports.get("confluence")
        confluence = confluence if isinstance(confluence, ConfluenceTransport) else None
        plane = transports.get("plane")
        plane = plane if isinstance(plane, PlaneTransport) else None

        if confluence is getattr(
            self, "_confluence_transport", None
        ) and plane is getattr(self, "_plane_transport", None):
            return

        self._confluence_transport = confluence
        self._plane_transport = plane
        backend = self._install_knowledge_backend()
        turn_engine = getattr(self, "turn_engine", None)
        if turn_engine is not None:
            turn_engine.set_knowledge_searcher(self._knowledge_searcher)
        if self._tool_skill_sync_worker is not None:
            # Re-seed from the new backend.  ``seed`` replaces
            # wholesale, so old-backend skills drop out WHEN a walk
            # lands — the kick retries with backoff, and if every
            # attempt fails it logs ``tool_skill_resync_exhausted``
            # loudly while the registry keeps the old skills (better
            # than an empty prompt surface; the operator is told).
            self._kick_tool_skill_resync()
        elif len(self._prompt_skill_registry):
            # No backend (or sync disabled) left to source skills from
            # — a registry serving a removed backend's prose would be
            # stale forever, so clear it.
            self._prompt_skill_registry.seed([])
            logger.info("tool_skill_registry_cleared", reason="no_sync_worker")
        logger.info("knowledge_backend_refreshed_live", backend=backend)

    def _refresh_confluence_routing(self, confluence_transport: Any) -> None:
        """Re-seed a freshly-rebuilt ConfluenceTransport's routing state.

        Restores the handle registry, the space-key→lead map, and the
        tool-skill notification-exclusion set — the same wiring
        ``start()`` does — so Confluence webhook routing survives a live
        ``integrations`` PUT that rebuilds the transport.  The knowledge
        machinery (searcher + sync worker + index callback) is
        reconciled by :meth:`_refresh_knowledge_backend` below.
        """
        from crewlet.config import build_space_key_lead_map
        from crewlet.notifications.transports.confluence import ConfluenceTransport

        if not isinstance(confluence_transport, ConfluenceTransport):
            return
        if self.handle_registry is not None:
            confluence_transport.set_handle_registry(self.handle_registry)
        # Always push the freshly built map — an empty map must CLEAR
        # live routing (see ``_refresh_jira_routing``).
        confluence_transport.set_space_key_leads(build_space_key_lead_map(self.org))
        skill_space = getattr(self, "_tool_skill_space_key", "")
        if skill_space:
            confluence_transport.set_notification_excluded_spaces([skill_space])
        # Rebuild the searcher / sync worker / index callback against
        # the (possibly new) transport instance — no-op when unchanged.
        self._refresh_knowledge_backend()
        logger.info("confluence_routing_refreshed_live")

    def _refresh_plane_routing(self, plane_transport: Any) -> None:
        """Re-seed a freshly-rebuilt (or org-stale) PlaneTransport's
        routing state.

        Restores the handle registry, the project-identifier→lead map,
        and the tool-skill notification-exclusion set — the same wiring
        ``start()`` does — so Plane webhook routing survives a live
        ``integrations`` PUT that rebuilds the transport and an org
        edit that changes the lead map.  The knowledge machinery
        (searcher + sync worker + index callback) is reconciled by
        :meth:`_refresh_knowledge_backend`.
        """
        from crewlet.config import build_plane_project_lead_map
        from crewlet.notifications.transports.plane import PlaneTransport

        if not isinstance(plane_transport, PlaneTransport):
            return
        if self.handle_registry is not None:
            plane_transport.set_handle_registry(self.handle_registry)
        # Always push the freshly built map — an empty map must CLEAR
        # live routing (see ``_refresh_jira_routing``).
        plane_transport.set_project_leads(build_plane_project_lead_map(self.org))
        # ``getattr`` guard: a transport swap can run before the spawn
        # cascade assigns ``_tool_skill_project_key`` (same reason
        # ``_refresh_confluence_routing`` guards its space key).
        skill_project = getattr(self, "_tool_skill_project_key", "")
        if skill_project:
            plane_transport.set_notification_excluded_projects([skill_project])
        # Rebuild the searcher / sync worker / index callback against
        # the (possibly new) transport instance — no-op when unchanged.
        self._refresh_knowledge_backend()
        logger.info("plane_routing_refreshed_live")

    async def _refresh_role_external_handles(
        self, only_roles: set[str] | None = None
    ) -> None:
        """Re-run Jira + GitHub + GitLab + Plane per-agent handle
        registration on the current org.  Called after a role is added
        live so the new agent's external identities land on the running
        handle registry without touching the integration's CompanyConfig
        fields (which the org-diff handler doesn't see directly).

        ``only_roles`` scopes the MCP identity lookups to the named
        roles — the role-add path passes the just-added set so a
        per-entity bootstrap issues one lookup per new agent instead of
        re-resolving every already-registered role on each
        ``POST /config/roles``.
        """
        if self.handle_registry is None or self.mcp_bridge is None:
            return

        # Human contact IDs are NOT refreshed here — the single home
        # for that reconcile is ``_apply_org_diff``'s unconditional
        # post-swap call (this method's only caller, ten lines up).

        # Jira and GitHub refreshes hit independent MCP servers, so
        # fire them concurrently -- per-entity bootstrap halves the
        # handle-refresh latency over a 50-role org with both
        # integrations.

        async def _refresh_jira() -> None:
            # Jira: present iff an ``atlassian`` MCP server is declared
            # in ``mcp_servers`` (the per-role Jira identity surface).
            if not any(c.name == "atlassian" for c in self._mcp_configs):
                return
            try:
                await register_jira_accounts_from_org(
                    self.handle_registry,
                    self.org,
                    mcp_bridge=self.mcp_bridge,
                    only_roles=only_roles,
                )
                logger.info("jira_handles_refreshed_live")
            except Exception as exc:
                logger.error("jira_handle_refresh_failed", error=str(exc))

        async def _refresh_github() -> None:
            # GitHub: enabled iff the engine captured a github config
            # at integrations setup.
            if self._github_config is None or not self._github_config.enabled:
                return
            try:
                await register_github_accounts_from_org(
                    self.handle_registry,
                    self.org,
                    mcp_bridge=self.mcp_bridge,
                    only_roles=only_roles,
                )
                logger.info("github_handles_refreshed_live")
            except Exception as exc:
                logger.error("github_handle_refresh_failed", error=str(exc))

        async def _refresh_gitlab() -> None:
            # GitLab: enabled iff the engine captured a gitlab config at
            # integrations setup.  Resolution is REST (GET /user), so —
            # unlike Jira/GitHub — it needs no MCP server to be running.
            if self._gitlab_config is None or not self._gitlab_config.enabled:
                return
            try:
                await register_gitlab_accounts_from_org(
                    self.handle_registry,
                    self.org,
                    gitlab_config=self._gitlab_config,
                    only_roles=only_roles,
                )
                logger.info("gitlab_handles_refreshed_live")
            except Exception as exc:
                logger.error("gitlab_handle_refresh_failed", error=str(exc))

        async def _refresh_plane() -> None:
            # Plane: enabled iff the engine captured a plane config at
            # integrations setup.  Resolution is REST (GET /users/me/),
            # so — like GitLab — it needs no MCP server to be running.
            if self._plane_config is None or not self._plane_config.enabled:
                return
            try:
                await register_plane_accounts_from_org(
                    self.handle_registry,
                    self.org,
                    plane_config=self._plane_config,
                    only_roles=only_roles,
                )
                logger.info("plane_handles_refreshed_live")
            except Exception as exc:
                logger.error("plane_handle_refresh_failed", error=str(exc))

        await asyncio.gather(
            _refresh_jira(), _refresh_github(), _refresh_gitlab(), _refresh_plane()
        )

    def _get_running_slack_transport(self) -> Any:
        """Return the running ``SlackTransport`` or ``None``."""
        if self.notification_service is None:
            return None
        from crewlet.notifications.transports.slack import SlackTransport

        slack_transport = self.notification_service.transports.get("slack")
        if not isinstance(slack_transport, SlackTransport):
            return None
        return slack_transport

    def _refresh_slack_apps(self) -> None:
        """Re-register per-agent Slack apps on the running SlackTransport.

        ``register_slack_apps_from_org`` only fires once at engine boot.
        Called after the SlackTransport is rebuilt by a live integrations
        PUT, so the new (empty) ``_apps`` map is re-seeded from the
        current org snapshot.  Idempotent: ``register_app`` overwrites
        by handle.
        """
        slack_transport = self._get_running_slack_transport()
        if slack_transport is None:
            return
        if self.handle_registry is not None:
            slack_transport.set_handle_registry(self.handle_registry)
        try:
            register_slack_apps_from_org(slack_transport, self.org)
            logger.info("slack_apps_refreshed_live")
        except Exception as exc:
            logger.error("slack_apps_refresh_failed", error=str(exc))

    def _register_role_slack_app(self, role: Any) -> None:
        """Register a single role's Slack app on the running transport.

        Used from ``_apply_org_diff`` role-add branch, where the new
        role isn't yet in ``self.org`` (the swap happens at the end
        of that method) so ``_refresh_slack_apps`` would miss it.
        ``role`` is the already-parsed ``Role`` object we just
        spawned an agent for.
        """
        slack_transport = self._get_running_slack_transport()
        if slack_transport is None or not role.slack:
            return
        from crewlet.notifications.transports.slack import SlackAppConfig

        if self.handle_registry is not None:
            slack_transport.set_handle_registry(self.handle_registry)
        resolved = resolve_env_vars(role.slack)
        bot_token = resolved.get("bot_token", "")
        if not bot_token:
            logger.warning("slack_bot_token_empty", role=role.name)
            return
        try:
            slack_transport.register_app(
                role.get_handle(),
                SlackAppConfig(
                    bot_token=bot_token,
                    signing_secret=resolved.get("signing_secret", ""),
                    channel=resolved.get("channel", ""),
                ),
            )
            logger.info(
                "slack_app_registered_live",
                handle=role.get_handle(),
                role=role.name,
            )
        except Exception as exc:
            logger.error("slack_app_register_failed", role=role.name, error=str(exc))

    def _get_running_mattermost_transport(self) -> Any:
        """Return the running ``MattermostTransport`` or ``None``."""
        if self.notification_service is None:
            return None
        from crewlet.notifications.transports.mattermost import MattermostTransport

        transport = self.notification_service.transports.get("mattermost")
        if not isinstance(transport, MattermostTransport):
            return None
        return transport

    def _refresh_mattermost_bots(self) -> None:
        """Re-register per-agent bots on a rebuilt MattermostTransport.

        A fresh transport starts with an empty bot map AND no websocket
        fleet, so without this a live integrations PUT leaves Mattermost
        entirely deaf until a restart.  The queue has to be re-supplied
        too: the fleet is built by ``transport.start()``, and refuses to
        start without one — which is why every caller must run this
        BEFORE starting the transport, not after.  Nothing starts a
        transport a second time.
        """
        transport = self._get_running_mattermost_transport()
        if transport is None:
            return
        if self.handle_registry is not None:
            transport.set_handle_registry(self.handle_registry)
        transport.set_event_queue(self.event_queue)
        try:
            register_mattermost_bots_from_org(transport, self.org)
            logger.info("mattermost_bots_refreshed_live")
        except Exception as exc:
            logger.error("mattermost_bots_refresh_failed", error=str(exc))

    async def _register_role_mattermost_bot(self, role: Any) -> None:
        """Register one role's Mattermost bot on the running transport.

        Used from the ``_apply_org_diff`` role-add branch, where the new
        role is not yet in ``self.org`` so ``_refresh_mattermost_bots``
        would miss it.  Registering here also opens that seat's websocket
        immediately — the fleet starts a connection for any seat
        registered while it is running — so a live-added agent is
        reachable on its first turn rather than after the next restart.
        """
        transport = self._get_running_mattermost_transport()
        if transport is None or not role.mattermost:
            return
        from crewlet.notifications.transports.mattermost import MattermostBotConfig

        if self.handle_registry is not None:
            transport.set_handle_registry(self.handle_registry)
        resolved = resolve_env_vars(role.mattermost)
        bot_token = resolved.get("bot_token", "")
        handle = role.get_handle()
        if not bot_token:
            logger.warning("mattermost_bot_token_empty", role=role.name)
            return
        try:
            transport.register_bot(
                handle,
                MattermostBotConfig(
                    bot_token=bot_token,
                    username=resolved.get("username", "") or handle,
                    channel=resolved.get("channel", ""),
                ),
            )
            fleet = transport.fleet
            if fleet is not None:
                await fleet.register_seat(handle, bot_token)
            logger.info("mattermost_bot_registered_live", handle=handle, role=role.name)
        except Exception as exc:
            logger.error(
                "mattermost_bot_register_failed", role=role.name, error=str(exc)
            )

    async def _refresh_github_handles(self, new: Any) -> None:
        """Re-register GitHub usernames from the org for webhook routing."""
        if self.handle_registry is None or self.mcp_bridge is None:
            return
        if new.integrations.github is None or not new.integrations.github.enabled:
            return
        try:
            await register_github_accounts_from_org(
                self.handle_registry,
                self.org,
                mcp_bridge=self.mcp_bridge,
            )
            logger.info("github_handles_refreshed_live")
        except Exception as exc:
            logger.error("github_handle_refresh_failed", error=str(exc))

    async def _refresh_gitlab_handles(self, new: Any) -> None:
        """Re-register GitLab usernames from the org for webhook routing.

        Called from the integrations diff *after* ``self._gitlab_config``
        has been rebuilt from ``new``; that resolved config (its ``url``
        drives the ``GET /user`` calls) is the source of truth here.
        """
        if self.handle_registry is None:
            return
        if self._gitlab_config is None or not self._gitlab_config.enabled:
            return
        try:
            await register_gitlab_accounts_from_org(
                self.handle_registry,
                self.org,
                gitlab_config=self._gitlab_config,
            )
            logger.info("gitlab_handles_refreshed_live")
        except Exception as exc:
            logger.error("gitlab_handle_refresh_failed", error=str(exc))

    async def _refresh_plane_handles(self, new: Any) -> None:
        """Re-register Plane user UUIDs from the org for webhook routing.

        Called from the integrations diff *after* ``self._plane_config``
        has been rebuilt from ``new``; that resolved config (its ``url``
        drives the ``GET /users/me/`` calls) is the source of truth
        here.
        """
        if self.handle_registry is None:
            return
        if self._plane_config is None or not self._plane_config.enabled:
            return
        try:
            await register_plane_accounts_from_org(
                self.handle_registry,
                self.org,
                plane_config=self._plane_config,
            )
            logger.info("plane_handles_refreshed_live")
        except Exception as exc:
            logger.error("plane_handle_refresh_failed", error=str(exc))

    async def _apply_extensions_live(
        self,
        new_extensions: list[Any],
        *,
        old_cfg_entries: list[dict[str, Any]],
        new_cfg_entries: list[dict[str, Any]],
    ) -> None:
        """Unregister extensions absent in the new list; register
        extensions that weren't present before; restart only the
        same-name extensions whose YAML settings actually differ.

        Identity is by ``extension.name``.  Settings comparison uses
        the raw config dicts (``[{"name": {settings...}}, ...]``) so
        we don't restart an extension whose config is unchanged just
        because some OTHER extension in the list was edited.  The
        caller (``_apply_restart_required_diff``) already gates this
        whole method on ``old.extensions != new.extensions``, so a
        true no-op apply doesn't reach here.
        """
        ctx = self._build_extension_context()
        old_by_name = {e.name: e for e in self._extension_manager.extensions}
        new_by_name = {e.name: e for e in new_extensions}
        removed_names = old_by_name.keys() - new_by_name.keys()
        added_names = new_by_name.keys() - old_by_name.keys()

        for name in removed_names:
            await self._extension_manager.unregister(old_by_name[name], ctx)

        for name in added_names:
            ext = new_by_name[name]
            try:
                await self._extension_manager.register(ext, ctx)
                await ext.on_engine_start(ctx)
                logger.info("extension_started_live", extension=name)
            except Exception as exc:
                logger.error(
                    "extension_register_failed_live",
                    extension=name,
                    error=str(exc),
                )

        old_cfg_by_name = _index_extension_entries(old_cfg_entries)
        new_cfg_by_name = _index_extension_entries(new_cfg_entries)
        for name in old_by_name.keys() & new_by_name.keys():
            if old_cfg_by_name.get(name) == new_cfg_by_name.get(name):
                continue  # unchanged — leave the live instance alone
            old_ext = old_by_name[name]
            new_ext = new_by_name[name]
            await self._extension_manager.unregister(old_ext, ctx)
            try:
                await self._extension_manager.register(new_ext, ctx)
                await new_ext.on_engine_start(ctx)
                logger.info("extension_restarted_live", extension=name)
            except Exception as exc:
                logger.error(
                    "extension_restart_failed_live",
                    extension=name,
                    error=str(exc),
                )

    def _build_sandbox_pending_store(self) -> Any:
        """Postgres-backed pending-run store when a DB is configured, else
        an in-memory one (single-process / tests)."""
        from crewlet.db.client import Database
        from crewlet.sandbox.pending_store import (
            MemoryPendingSandboxRunStore,
            PostgresPendingSandboxRunStore,
        )

        if isinstance(self.storage, Database):
            return PostgresPendingSandboxRunStore(self.storage)
        return MemoryPendingSandboxRunStore()

    def _build_sandbox_otel_receiver(self) -> Any:
        """Build the engine-fronted OTLP receiver, or ``None``.

        Wired only when ``CREWLET_SANDBOX_OTEL_RECEIVER_URL`` is set (the
        externally-reachable engine API base the sandbox exports to). It
        forwards to the engine's own OTLP backend (``OTEL_EXPORTER_OTLP_*``),
        adding the upstream auth here so the backend token never reaches the
        sandbox. ``None`` → the sandbox uses a directly-configured collector
        endpoint (or no telemetry).
        """
        from crewlet.sandbox.otel import build_sandbox_otel_receiver

        return build_sandbox_otel_receiver(getattr(self, "_bootstrap", None))

    def _role_for_agent_id(self, agent_id: str) -> str:
        """Role name for a live agent id, or ``""``.

        The budget cascade is keyed by ``AgentInstance.id_str`` while
        every consumer of the meter is keyed by role, so the engine --
        which holds both -- does the mapping once here rather than
        making the API re-derive it.  Re-derivation would be a second
        identity path, and the two diverge after a live handle edit
        (the instance keeps the handle it was constructed with).
        """
        agent = self.agent_pool.get_by_id(agent_id)
        return agent.role_name if agent is not None else ""

    async def _start_sandbox_coordinator(self) -> None:
        """Build, subscribe, and recover the sandbox coordinator.

        No-op when no sandbox provider is configured or the turn engine
        isn't built yet. Shares the pending store with the turn engine's
        kick-off so completion reads what the kick-off wrote.
        """
        if self._sandbox_manager is None or self.turn_engine is None:
            return
        from crewlet.db.client import Database
        from crewlet.db.token_usage import TokenUsageRepository
        from crewlet.sandbox.coordinator import SandboxCoordinator

        if self._sandbox_pending_store is None:
            self._sandbox_pending_store = self._build_sandbox_pending_store()
        token_usage_repo = (
            TokenUsageRepository(self.storage)
            if isinstance(self.storage, Database)
            else None
        )
        self._sandbox_coordinator = SandboxCoordinator(
            event_queue=self.event_queue,
            pending_store=self._sandbox_pending_store,
            manager=self._sandbox_manager,
            get_agent=self.agent_pool.get_by_handle,
            get_turn_engine=lambda: self.turn_engine,
            get_org=lambda: self.org,
            budget_manager=self.budget_manager,
            token_usage_repo=token_usage_repo,
        )
        try:
            # No fleet-wide subscribe and no fleet-wide recover: both are
            # per-seat, done by the acquire hook, because only the node
            # that owns a seat can resume its runs. This path can start
            # AFTER seats were claimed (a provider arriving late), so
            # catch up on whatever is already held.
            held = self._seat_host.held_handles if self._seat_host is not None else ()
            for handle in held:
                await self._sandbox_coordinator.recover_seat(
                    handle,
                    owner=self._incarnation,
                    epoch=self._seat_host.epoch_for(handle) or 0,
                )
                await self._sandbox_coordinator.attach_seat(handle)
            logger.info("sandbox_coordinator_started", seats=len(held))
        except Exception as exc:
            logger.error("sandbox_coordinator_start_failed", error=str(exc))

        # Completion poll — THE completion signal for detached jobs, and
        # the running boxes' keepalive tick.
        from crewlet.sandbox.waiter import SandboxWaiter

        self._sandbox_waiter = SandboxWaiter(
            event_queue=self.event_queue,
            pending_store=self._sandbox_pending_store,
            manager=self._sandbox_manager,
            # One waiter per company, not per node: it polls every live
            # box, so N nodes running it means N reconnects per box per
            # tick and N reapers racing to expire the same paused one.
            claim_duty=lambda: self.claim_worker_duty("sandbox-waiter"),
        )
        try:
            await self._sandbox_waiter.start()
        except Exception as exc:
            logger.error("sandbox_waiter_start_failed", error=str(exc))

    def _build_turn_engine(
        self,
        *,
        summarize_enabled: bool | None = None,
        summarize_max_tokens: int | None = None,
    ) -> None:
        """Construct ``self.turn_engine`` from the current subsystem state.

        Factored out of ``start()`` step 7 so the live-config provider
        diff can build the engine when the first LLM provider arrives
        after first activation (per-entity bootstrap: an empty stub
        boots with zero providers, ``PUT /config/llm-providers`` adds
        them later).  Without this, ``self.turn_engine`` stays ``None``,
        agent inbox handlers are never subscribed, and routed
        notifications sit unconsumed.

        ``summarize_*`` default to the values derived from the active
        learning config when not supplied by the ``start()`` caller.
        """
        from crewlet.agent.turn_settings import TurnEngineSettings
        from crewlet.config import TurnEngineConfig
        from crewlet.db.client import Database
        from crewlet.db.token_usage import TokenUsageRepository

        if summarize_enabled is None or summarize_max_tokens is None:
            lrn_cfg = self._learning_config
            reflect_cfg = (
                getattr(lrn_cfg, "reflect", None) if lrn_cfg is not None else None
            )
            if summarize_enabled is None:
                summarize_enabled = bool(
                    reflect_cfg.summarize_episodes if reflect_cfg is not None else True
                )
            if summarize_max_tokens is None:
                summarize_max_tokens = int(
                    reflect_cfg.summarize_max_tokens if reflect_cfg is not None else 400
                )

        token_usage_repo: TokenUsageRepository | None = None
        if isinstance(self.storage, Database):
            token_usage_repo = TokenUsageRepository(self.storage)

        # Live-reconfigurable turn-engine settings cell —
        # apply_config updates it via self.turn_engine._settings.set().
        te_cfg = getattr(self, "_turn_engine_config", None) or TurnEngineConfig()
        te_settings = TurnEngineSettings(te_cfg)
        self.turn_engine = TurnEngine(
            llm_providers=self._llm_providers,
            tool_registry=self.tool_registry,
            event_queue=self.event_queue,
            role_mcp_tools=self._role_mcp_tools,
            settings=te_settings,
            storage=self.storage,
            knowledge_searcher=getattr(self, "_knowledge_searcher", None),
            concurrency=self.concurrency,
            budget_manager=self.budget_manager,
            observability=self.observability,
            notification_service=self.notification_service,
            a2a_service=self.a2a_service,
            handle_registry=self.handle_registry,
            execution_tracker=self.execution_tracker,
            token_usage_repo=token_usage_repo,
            episode_store=self._episode_store,
            counterparty_store=getattr(self, "_counterparty_store", None),
            synthesized_skill_store=getattr(self, "_synthesized_skill_store", None),
            agent_diary=getattr(self, "_agent_diary", None),
            onboarding_marker_store=getattr(self, "_onboarding_marker_store", None),
            episode_recall_summarize=summarize_enabled,
            episode_recall_summarize_max_tokens=summarize_max_tokens,
            prompt_skill_registry=self._prompt_skill_registry,
            sandbox_manager=self._sandbox_manager,
            sandbox_pending_store=self._sandbox_pending_store,
            sandbox_mcp_servers=getattr(self, "_mcp_configs", None),
            sandbox_otel_receiver=self._sandbox_otel_receiver,
            llm_provider_configs=self._llm_provider_configs,
            # The in-turn fence. Read live rather than captured, because
            # the host is built during ``start`` and the turn engine can
            # be built before or after it.
            seat_owner=_SeatOwnerView(self),
        )

    async def _ensure_turn_engine_after_providers(self) -> None:
        """Build the turn engine and un-park the seats once the first LLM
        provider lands post-activation.

        No-op when the engine already exists or no providers are
        configured.

        Iterates the seats this node OWNS, not the agent pool. The pool
        retains terminated instances, so walking it re-subscribed
        decommissioned seats — resurrecting a dead seat's consumer with a
        handler closed over a TERMINATED instance. And the resume names
        the park's own reason, so it cannot release a live sandbox hold
        on a seat that is mid-run.
        """
        if self.turn_engine is not None or not self._llm_providers:
            return
        self._build_turn_engine()
        logger.info("turn_engine_built_live")
        held = self._seat_host.held_handles if self._seat_host is not None else ()
        for handle in held:
            # Events that arrived while there was NO turn engine were
            # parked (subscription paused + requeued by the inbox
            # handler) — now that turns can run, let them flow. No-op for
            # never-paused subscriptions.
            await self.event_queue.resume_topic(
                agent_inbox_topic(handle),
                agent_inbox_group(handle),
                reason=_NO_TURN_ENGINE_PAUSE_REASON,
            )
            logger.info("agent_inbox_unparked_live", handle=handle)
        # The sandbox coordinator needs the (now-built) turn engine to
        # dispatch completion turns; start it on this late path too.
        if self._sandbox_coordinator is None:
            await self._start_sandbox_coordinator()

    def _apply_providers_diff(self, old: Any, new: Any) -> bool:
        """Re-instantiate LLM and embedding providers when changed.

        Returns True if any provider was rebuilt.

        The provider dict is swapped in place; in-flight LLM calls
        finish on the old client (captured in their call frames),
        next turn picks up the new one.  TurnEngine reads providers
        via ``self._llm_providers`` (dict reference, not snapshot)
        so the swap is visible without further wiring.
        """
        from crewlet.engine_builders import (
            build_embedding_provider,
            build_llm_providers,
            build_sandbox_manager,
        )

        changed = False

        if old.providers.llm != new.providers.llm:
            new_providers = build_llm_providers(new)
            # In-place mutation preserves the dict identity that
            # TurnEngine captured at construction time.
            self._llm_providers.clear()
            self._llm_providers.update(new_providers)
            # Keep the sandbox backend's provider-config view in sync
            # — same in-place mutation so TurnEngine's by-reference
            # ``_llm_provider_configs`` sees the new creds next turn.
            self._llm_provider_configs.clear()
            self._llm_provider_configs.update(new.providers.llm)
            logger.info(
                "llm_providers_updated",
                added=list(new_providers.keys() - old.providers.llm.keys()),
                removed=list(old.providers.llm.keys() - new_providers.keys()),
            )
            changed = True

        if old.providers.sandbox != new.providers.sandbox:
            # Rebuild the manager (a single object, not a by-reference
            # dict) and publish it to the running TurnEngine.  ``None``
            # disables the backend; a re-enable wires it back without an
            # engine restart.
            self._sandbox_manager = build_sandbox_manager(new)
            if self.turn_engine is not None:
                self.turn_engine.set_sandbox_manager(self._sandbox_manager)
            if self._sandbox_coordinator is not None and self._sandbox_manager:
                self._sandbox_coordinator.set_manager(self._sandbox_manager)
            logger.info(
                "sandbox_provider_updated",
                enabled=self._sandbox_manager is not None,
            )
            changed = True

        if old.providers.embeddings != new.providers.embeddings:
            self._embeddings = build_embedding_provider(new)
            # The embeddings provider is wired DEEPLY into the learning
            # subsystem at boot — EpisodeStore, AgentDiary and the
            # reflect-engine tools all capture it by reference inside the
            # spawn cascade.  Re-instantiating it here updates
            # ``self._embeddings`` for a future restart but does NOT
            # rewire those already-built consumers (they keep the old
            # provider).  Worse, the pgvector column width is fixed at
            # migration time, so a model with different dimensions would
            # need a schema migration too.  Match the ``_apply_learning_live``
            # contract: store the new provider and warn loudly that a
            # restart is required, rather than silently leaving diary /
            # episodes on the prior embeddings.
            if self._tier_b_done:
                logger.warning(
                    "embeddings_change_restart_required",
                    hint=(
                        "embeddings provider changed; the running "
                        "agent_diary / episode store / reflect engine "
                        "continue on the prior provider (and pgvector "
                        "column width) until the engine is restarted"
                    ),
                )
            else:
                logger.info("embeddings_updated_pre_cascade")
            changed = True

        return changed

    def _apply_scalars_diff(self, old: Any, new: Any) -> bool:
        """Update simple scalar engine state that doesn't need rewiring."""
        changed = False
        if old.integrations.forge_app_id != new.integrations.forge_app_id:
            from crewlet.engine_builders import resolve_forge_app_id

            self._forge_app_id = resolve_forge_app_id(new)
            logger.info("forge_app_id_updated")
            changed = True
        if old.notification_rate_limit != new.notification_rate_limit:
            self._notification_rate_limit = new.notification_rate_limit
            # Propagate onto the running service so the new per-agent cap
            # takes effect on the next notification — ``_check_rate_limit``
            # reads ``service._rate_limit`` live.  Without this the change
            # only landed on the engine mirror and was ignored until a
            # restart.
            if self.notification_service is not None:
                self.notification_service.rate_limit = new.notification_rate_limit
            logger.info("notification_rate_limit_updated")
            changed = True
        if (
            old.notification_coalesce_window_seconds
            != new.notification_coalesce_window_seconds
        ):
            # The inbox batch consume loops read the shared BatchOptions
            # at the start of every collection cycle, so mutating the
            # field takes effect on the next batch with no
            # re-subscription.
            self._inbox_batch_options.linger_seconds = (
                new.notification_coalesce_window_seconds
            )
            logger.info("notification_coalesce_window_updated")
            changed = True
        if old.notification_coalesce_max_batch != new.notification_coalesce_max_batch:
            self._inbox_batch_options.max_batch = new.notification_coalesce_max_batch
            logger.info("notification_coalesce_max_batch_updated")
            changed = True
        return changed

    def _apply_turn_engine_diff(self, old: Any, new: Any) -> bool:
        """Push the new ``TurnEngineConfig`` into the live settings cell.

        Returns True if the config was swapped.

        Reads through :class:`TurnEngineSettings` so in-flight turns
        finish on whatever snapshot the LLM loop captured locally;
        next turn picks up the new settings via the property
        accessors on :class:`TurnEngine`.
        """
        if old.turn_engine == new.turn_engine:
            return False
        # Update the stored config on the engine so future
        # TurnEngine reconstructions (e.g. tests) see the new value.
        self._turn_engine_config = new.turn_engine
        if self.turn_engine is not None:
            self.turn_engine._settings.set(new.turn_engine)
        logger.info("turn_engine_config_updated")
        return True

    def _apply_budgets_diff(self, old: Any, new: Any) -> bool:
        """Update org + per-role token budgets in place.

        Used-tokens counters survive — :meth:`BudgetManager.update_*`
        preserve the existing ``TokenBudget`` instances rather than
        replacing them.
        """
        if old.token_budget == new.token_budget:
            return False
        self.budget_manager.update_org_budget(new.token_budget)
        logger.info(
            "org_token_budget_updated",
            old=old.token_budget,
            new=new.token_budget,
        )
        return True

    # ── seat ownership ───────────────────────────────────────────────

    def _resolve_node_profile(self) -> Any:
        """Read this node's roles and labels out of Tier A.

        Cached on the engine because it is consulted on every duty claim
        and every presence renew, and because a node's roles must not
        change under a running process: the fleet reads them off a lease
        that this node re-sends on a heartbeat, so a value that drifted
        mid-run would be advertised without anything having restarted.
        """
        from crewlet.seat.placement import NodeProfile, parse_roles

        bootstrap = getattr(self, "_bootstrap", None)
        node_cfg = getattr(bootstrap, "node", None) if bootstrap is not None else None
        self._node_profile = NodeProfile(
            node_id=self._node_id,
            roles=parse_roles([str(r) for r in getattr(node_cfg, "roles", [])] or None),
            labels=dict(getattr(node_cfg, "labels", {}) or {}),
        )
        return self._node_profile

    @property
    def node_profile(self) -> Any:
        """What this node is. Never ``None``.

        Resolved from Tier A on demand, not only from ``start``. The
        fallback below is for an engine that genuinely has no Tier A — a
        directly-constructed one, which has always been the all-roles
        single-process default. Applying that fallback to an engine that
        *has* Tier A merely because it has not started yet answers
        ``runs_role`` with every role for a node whose config subtracts
        two of them, which is the opposite of what the operator wrote.
        """
        from crewlet.seat.placement import NodeProfile

        if self._node_profile is None:
            if getattr(self, "_bootstrap", None) is not None:
                return self._resolve_node_profile()
            self._node_profile = NodeProfile(node_id=self._node_id)
        return self._node_profile

    def runs_role(self, role: Any) -> bool:
        """Whether this node performs a given :class:`NodeRole`."""
        return role in self.node_profile.roles

    def _seat_placement(self, handle: str) -> Any:
        """Where the seat behind ``handle`` may run, per the ACTIVE org.

        Read fresh on every sweep rather than snapshotted: a live config
        apply can move a pin, and a node holding a seat it no longer
        matches has to find out.
        """
        from crewlet.seat.placement import ANYWHERE

        role = self.org.agent_seat_by_handle(handle)
        placement = getattr(role, "placement", None) if role is not None else None
        return placement.to_placement() if placement is not None else ANYWHERE

    def _build_seat_host(self) -> Any:
        """Construct the placement host. Always — including single-node.

        Backed by a real ``LeaseStore`` when a database is configured and
        by a ``MemoryLeaseStore`` otherwise, so the single-node case is
        the *degenerate* case of the fleet case rather than a second code
        path. Every seat is then established through one sequence, and
        the ordering bug that lived in the live-add path cannot come back
        by only fixing boot.

        A memory-backed store is private to this process, so every node
        using one believes it owns the whole company. That is correct for
        one node and catastrophic for two, which is why it is said out
        loud at boot rather than left as a debug line.

        An explicit ``lease_store=`` overrides the derivation, for the
        caller that runs several engines against one fleet in one
        process — an embedding, a fleet test.  Each still mints its own
        incarnation, so they are peers to each other exactly as separate
        processes would be, and the warning above does not apply: the
        store is shared, so the fleet is real.
        """
        from crewlet.config import new_node_incarnation
        from crewlet.db.client import Database
        from crewlet.db.leases import LeaseStore, MemoryLeaseStore
        from crewlet.seat.host import SeatHost

        bootstrap = getattr(self, "_bootstrap", None)
        # A FRESH incarnation per engine, not the process-cached one.
        # Two engines in one process are two lease holders; sharing an
        # identity would let each renew the other's lease and put both on
        # one seat at the same fencing epoch — the exact hole the
        # incarnation exists to close.
        if not self._incarnation:
            self._incarnation = new_node_incarnation(bootstrap)
        if self._lease_store is not None:
            leases: Any = self._lease_store
        elif isinstance(self.storage, Database):
            leases = LeaseStore(self.storage)
        else:
            leases = MemoryLeaseStore()
            logger.warning(
                "seat_placement_is_process_local",
                node=self._node_id,
                hint=(
                    "no database configured, so seat leases are held in "
                    "this process only and it will claim every seat. "
                    "Correct for a single node; running a second node "
                    "against the same broker in this mode gives two "
                    "processes the same agents. Configure "
                    "providers.database.dsn to run a fleet."
                ),
            )
        return SeatHost(
            leases=leases,
            owner=self._incarnation,
            node_id=self._node_id,
            seats=self._agent_seat_handles,
            placement_of=self._seat_placement,
            profile=self.node_profile,
            on_acquire=self._acquire_seat,
            on_release=self._release_seat,
            on_admission=self._seat_admission_changed,
        )

    def _start_loop_watchdog(self) -> None:
        """Arm the one thing a stalled process can still do about itself.

        A wedged event loop cannot be signalled — ``call_soon_threadsafe``
        waits behind the same blockage — so no cross-thread mechanism can
        stop this node's seat work from the outside. What a thread *can*
        do unilaterally is end the process, and ending it is what the
        fleet needs: the leases lapse on their own and peers take the
        seats over, but the broker session does not lapse, so a
        wedged-but-alive node keeps its prefetch of those seats' messages
        for the full unacked-message timeout (~30 minutes, measured).
        Exiting collapses that to a 9 ms redelivery.

        The threshold is the seat lease TTL and is deliberately not a
        config knob: past it this node is provably not the owner, and
        letting the two numbers drift is precisely how a process gets to
        be simultaneously "not the owner" and "still holding the mail".

        Armed on every node, single or fleet. With one node there are no
        peers waiting on the prefetch, but a wedged engine is a dead
        engine either way, and exiting is what lets a supervisor notice.
        """
        if self._loop_watchdog is not None:
            return
        from crewlet.seat.host import SEAT_LEASE_TTL_SECONDS
        from crewlet.seat.watchdog import EventLoopWatchdog

        watchdog = EventLoopWatchdog(threshold_seconds=SEAT_LEASE_TTL_SECONDS)
        try:
            watchdog.start()
        except Exception as exc:  # pragma: no cover — a thread that will not start
            logger.warning("loop_watchdog_start_failed", error=str(exc))
            return
        self._loop_watchdog = watchdog

    async def _stop_loop_watchdog(self) -> None:
        watchdog, self._loop_watchdog = self._loop_watchdog, None
        if watchdog is None:
            return
        with contextlib.suppress(Exception):
            await watchdog.stop()

    async def _seat_admission_changed(self, handle: str, admitted: bool) -> None:
        """Stop or resume consuming a seat this node still holds.

        The lease store went unreachable, or came back. Either way the
        seat stays here — the row is untouched by a blip, and shedding on
        one would tear a healthy company down over a two-second outage —
        but ownership stops being *provable*, and a turn started without
        proof is a turn a peer may be running too.

        So the consumer is quiesced rather than detached: the attachment
        and the subscription survive, in-flight work finishes, and
        nothing new is fetched. The resume is the half that has to exist:
        a delivery arriving during the blip quiesces the attachment
        through ``DeferDelivery``, and with no inverse the node would
        come back healthy, still own the seat, still be attached to it,
        and never read from it again.

        Pause holds are deliberately untouched in both directions — a
        seat mid-sandbox is paused for its own reason, and lifting that
        here would deliver into a suspended turn.
        """
        topics = (
            (agent_inbox_topic(handle), agent_inbox_group(handle)),
            (agent_control_topic(handle), agent_control_group(handle)),
        )
        for topic, group in topics:
            try:
                if admitted:
                    await self.event_queue.unquiesce(topic, group)
                else:
                    await self.event_queue.quiesce(topic, group)
            except Exception:
                logger.exception(
                    "seat_admission_gate_failed",
                    seat=handle,
                    topic=topic,
                    admitted=admitted,
                )

    async def claim_worker_duty(self, duty: str, ttl_seconds: float = 45.0) -> bool:
        """Claim a fleet-wide singleton duty for this node, briefly.

        Some work belongs to the company rather than to a seat — polling
        every live sandbox box, sweeping dedupe TTLs, creating the seat
        subscriptions. Running it on every node is not merely wasteful,
        it races: N reapers deciding independently to expire the same
        paused box, N nodes reconnecting to the same sandbox each tick.

        Claimed per tick rather than held, so a node that dies mid-duty
        releases it by lapsing and a peer picks it up on its next tick,
        with no handoff protocol. Without a placement host — the
        single-node case — the answer is always yes: there is no fleet
        to be a singleton within.

        A node without the ``workers`` role never claims one. It does not
        merely lose the race, it does not enter it: the duty then belongs
        to whichever node the operator gave the role to, which is the
        entire point of subtracting it here. A fleet where NOBODY has the
        role runs none of these duties at all, so that shape is checked
        against live presence and reported — see
        :meth:`_check_fleet_roles`.
        """
        from crewlet.seat.placement import NodeRole

        if not self.runs_role(NodeRole.WORKERS):
            return False
        if self._seat_host is None:
            return True
        from crewlet.db.leases import worker_resource

        try:
            lease = await self._seat_host.leases.try_acquire(
                worker_resource(duty),
                owner=self._incarnation,
                ttl_seconds=ttl_seconds,
                preferred=self._node_id,
                # THIS build's protocol, not the store's default. A duty
                # lease written at the default sits in the table as a
                # live lower-protocol row, and the mixed-version gate
                # refuses every SEAT claim while one exists — so a node
                # would take a duty and then be unable to claim a single
                # seat, including on a fleet where it is the only node.
                # Invisible while PROTOCOL_VERSION was 1 and immediate
                # the moment it moved.
                protocol=self._seat_host.protocol,
                gated=False,
            )
        except Exception:
            logger.exception("worker_duty_claim_failed", duty=duty)
            return False
        return lease is not None

    def _agent_seat_handles(self) -> list[str]:
        """Every agent seat in the ACTIVE org, read fresh each sweep.

        A snapshot taken at construction would keep claiming seats a live
        config apply removed, and miss the ones it added.
        """
        return [r.get_handle() for r in self.org.all_roles() if not r.is_human]

    async def _ensure_seat_subscriptions(self) -> None:
        """Make every seat's inbox subscription exist, owner or not.

        The invariant an unowned seat rests on: a durable subscription
        retains what is published to it, so mail that lands during a
        lease gap, a claim ramp, or a full fleet restart is held rather
        than dropped on the floor — no dead letter, no producer error,
        nothing to alert on.

        Behind the ``seat-subscriptions`` singleton lease, because this
        only needs doing once per company and there is no reason for
        every node to walk every seat at every boot.
        """
        handles = self._agent_seat_handles()
        if not handles:
            return
        if not await self.claim_worker_duty("seat-subscriptions", ttl_seconds=60.0):
            logger.debug("seat_subscriptions_held_elsewhere")
            return
        created = 0
        for handle in handles:
            try:
                if await self.event_queue.ensure_subscription(
                    agent_inbox_topic(handle), agent_inbox_group(handle)
                ):
                    created += 1
                # The seat's control topic needs the same invariant: a
                # detached run outlives its node, so a completion can
                # land while the seat is between owners.
                if await self.event_queue.ensure_subscription(
                    agent_control_topic(handle), agent_control_group(handle)
                ):
                    created += 1
            except Exception:
                logger.exception("seat_subscription_ensure_failed", seat=handle)
        logger.info("seat_subscriptions_ensured", seats=len(handles), created=created)

    async def _acquire_seat(self, handle: str, lease: Any) -> None:
        """Establish a seat this node has just claimed, consumer LAST.

        The ordering is the whole method. A seat is *established* — its
        instance exists, its budget cap is seeded, its per-role MCP
        children are up, its interrupted sandbox run is recovered — and
        only then does it start receiving work. Attaching the consumer
        first means the seat can win a delivery and run its first turn
        with an empty tool surface, which is exactly what
        ``_spawn_role_live`` used to do (subscribe at 5823, MCP at 5833)
        while boot did the reverse.

        Raising is how a takeover reports failure: the host releases the
        lease, backs the seat off, and a peer gets a clear run at it.
        """
        role = self.org.agent_seat_by_handle(handle)
        if role is None:
            raise RuntimeError(
                f"claimed seat {handle!r} has no agent role in the active org"
            )
        agent = self.agent_pool.get_by_handle(handle)
        if agent is None:
            agent = await self.agent_pool.spawn_role(
                role, self.org, source="seat.acquire"
            )
        self._seed_seat_budget(role, self.org)

        if self._tier_b_done:
            await self._respawn_role_mcp(role)
            self._register_role_slack_app(role)
            await self._register_role_mattermost_bot(role)

        # Recover an interrupted detached sandbox run BEFORE the consumer
        # attaches: the run may have left the seat paused and parked, and
        # a delivery arriving first would start a turn beside it. Then
        # take over the seat's control topic, so completions for its runs
        # reach this node and only this node.
        if self._sandbox_coordinator is not None:
            await self._sandbox_coordinator.recover_seat(
                handle, owner=self._incarnation, epoch=lease.epoch
            )
            await self._sandbox_coordinator.attach_seat(handle)

        # LAST, and unconditionally. An owned seat is always attached:
        # making the attachment depend on the turn engine existing left a
        # seat owned in the lease table and deaf in the process, waiting
        # on a resume pass that keyed off the pool rather than off
        # ownership. A node booted with zero LLM providers parks its
        # deliveries in the handler instead, under its own pause reason.
        await self._subscribe_agent_inbox(agent, epoch=lease.epoch)
        logger.info("seat_established", seat=handle, epoch=lease.epoch)

    async def _release_seat(self, handle: str, lease: Any, reason: Any) -> None:
        """Give a seat up. What that means depends entirely on ``reason``.

        **Voluntary** (drain, role gone): quiesce so nothing new is
        picked up, let the in-flight turn finish under a bounded wait,
        then detach. The seat leaves cleanly and its work is not
        duplicated.

        **Fenced** (lease lost, acquire failed, posture): detach FIRST
        and abandon whatever is running. A peer may already be serving
        this seat, so waiting for a turn to finish only widens the
        window in which two nodes run one agent — and nothing is
        republished, which would hand that peer a second copy of work it
        is already doing.

        Either way this must be idempotent and tolerant of partial
        state: a failed acquire releases the same seat, so the MCP
        children may be half-spawned and the consumer may never have
        attached.
        """
        from crewlet.seat.host import ReleaseReason

        fenced = bool(getattr(reason, "fenced", False))
        if not fenced:
            # Stop taking new work, then let the running turn finish.
            with contextlib.suppress(Exception):
                await self.event_queue.quiesce(
                    agent_inbox_topic(handle), agent_inbox_group(handle)
                )
            if reason != ReleaseReason.ROLE_GONE:
                await self._drain_seat_by_handle(handle)

        # Detach before tearing anything else down: while the consumer is
        # attached this node is still the seat's owner in practice.
        # Raises SeatReleaseError if it cannot be proven, and the host
        # then keeps the lease rather than handing on a seat we may still
        # be consuming.
        await self._detach_agent_inbox(handle)

        if self._sandbox_coordinator is not None:
            with contextlib.suppress(Exception):
                await self._sandbox_coordinator.detach_seat(handle)
            with contextlib.suppress(Exception):
                await self._sandbox_coordinator.release_seat(handle)

        role = self.org.agent_seat_by_handle(handle)
        if role is not None:
            with contextlib.suppress(Exception):
                await self._stop_role_mcp(role)
        agent = self.agent_pool.get_by_handle(handle)
        if agent is not None:
            for issue_key in self.execution_tracker.get_issues(agent.id_str):
                self.execution_tracker.untrack(issue_key)
            with contextlib.suppress(Exception):
                await self.agent_pool.terminate(agent)
        logger.info("seat_relinquished", seat=handle, reason=str(reason))

    def _may_serve_seat(self, handle: str) -> bool:
        """Whether a NEW turn may start on this seat, right now.

        Freshness-based — see
        :meth:`~crewlet.seat.host.SeatHost.may_start`. A membership check
        would read a snapshot up to a full lease TTL stale, which is
        exactly the window it exists to close.
        """
        if self._seat_host is None:
            return True
        return self._seat_host.may_start(handle) is not None

    async def _drop_worked_triggers(
        self, agent: AgentInstance, events: list[Event]
    ) -> list[Event]:
        """Remove triggers a previous turn already finished.

        The window this closes: a turn completes, its outbound effects
        ship, and the node dies before the delivery is acked. At-least-
        once then hands the trigger to the seat's next owner, which
        re-runs the whole thing — a second Slack reply, a second Jira
        comment, a second push.

        Only trigger types that actually RUN a turn are consulted;
        anything else passes through untouched (see
        :data:`_LEDGERED_INBOX_TYPES` for which, and why).

        Fails OPEN, like the store: an unreadable ledger cannot tell you
        whether work was done, and the only safe answer to that is the
        one the engine gave before the ledger existed — run it.
        """
        if self._turn_completions is None or not events:
            return events
        candidates = [e for e in events if e.type in _LEDGERED_INBOX_TYPES]
        if not candidates:
            return events
        worked = await self._turn_completions.completed(
            agent.handle, [str(e.id) for e in candidates]
        )
        if not worked:
            return events
        kept = [e for e in events if str(e.id) not in worked]
        skipped = len(events) - len(kept)
        logger.info(
            "turn_trigger_already_worked",
            agent_handle=agent.handle,
            skipped=skipped,
            remaining=len(kept),
        )
        await self._publish_trigger_skipped(agent, events, worked)
        return kept

    async def _record_worked_triggers(
        self, agent: AgentInstance, events: list[Event]
    ) -> None:
        """Record the constituent triggers this turn finished.

        The CONSTITUENTS, never a derived id: a coalesced partition is
        merged into one digest before the turn runs and that digest is
        minted fresh on every coalesce, so a key taken from it would
        differ on every redelivery and match nothing.

        Called after the dispatch returns — which includes the sandbox
        SUSPEND, deliberately. Past the suspend the pending row's
        ``claim_for_resume`` is the at-most-once authority for the rest
        of that work, and the trigger itself is finished with.

        Fails open inside the store: the side effects already shipped,
        so failing to record them costs at most one duplicate turn —
        exactly the behaviour without a ledger.
        """
        if self._turn_completions is None or not events:
            return
        ids = [str(e.id) for e in events if e.type in _LEDGERED_INBOX_TYPES]
        if not ids:
            return
        epoch = 0
        if self._seat_host is not None:
            epoch = self._seat_host.epoch_for(agent.handle) or 0
        await self._turn_completions.record(
            agent.handle,
            ids,
            turn_id=str(getattr(agent, "current_turn_id", "") or ""),
            node=self._incarnation,
            owner_epoch=epoch,
        )

    async def _publish_trigger_skipped(
        self, agent: AgentInstance, events: list[Event], worked: set[str]
    ) -> None:
        """Announce a short-circuit, so the mechanism is auditable.

        Without it a skipped trigger is invisible in every operator
        surface the product has: the dashboard shows no turn, the logs
        show no error, and "the agent never answered" and "the agent
        already answered on a node that has since died" look identical.
        """
        from crewlet.events.types import TurnTriggerSkipped

        for event in events:
            if str(event.id) not in worked:
                continue
            with contextlib.suppress(Exception):
                await self.event_queue.publish(
                    "crewlet.events.turn_trigger_skipped",
                    TurnTriggerSkipped(
                        source="engine",
                        agent_handle=agent.handle,
                        agent_id=agent.id_str,
                        trigger_id=str(event.id),
                        trigger_type=event.type,
                        reason="already_worked",
                    ),
                )

    def _dedupe_inbox_events(
        self, agent: AgentInstance, events: list[Event]
    ) -> list[Event]:
        """Drop repeat ids from one drain. See the handler for why."""
        seen_ids: set[Any] = set()
        deduped: list[Event] = []
        for event in events:
            if event.id in seen_ids:
                logger.info(
                    "inbox_duplicate_dropped",
                    agent_handle=agent.handle,
                    event_type=event.type,
                    event_id=str(event.id),
                )
                continue
            seen_ids.add(event.id)
            deduped.append(event)
        return deduped

    async def _drain_seat_by_handle(self, handle: str) -> bool:
        """Bounded wait for a seat's in-flight turn, keyed by HANDLE.

        ``_drain_seat`` keys by role name because every config-apply
        primitive does; the seat host keys by handle because that is what
        a lease names. One mapping, in one place, rather than each caller
        guessing.
        """
        role = self.org.agent_seat_by_handle(handle)
        if role is None:
            return True
        return await self._drain_seat(role.name)

    async def _spawn_role_live(self, role: Any, org: Any) -> None:
        """Establish one agent seat into the live engine (apply_config path).

        Delegates the seat itself to the placement host so a live-added
        role goes through exactly the same establish-then-attach sequence
        as a takeover — this path used to invert it, subscribing the
        inbox before spawning the per-role MCP children.

        Without a claim the seat is simply not this node's to run: the
        sweep will pick it up here or a peer will.
        """
        self._seed_seat_budget(role, org)
        logger.info("seat_added_to_org", role=role.name, handle=role.get_handle())

    async def _decommission_role_live(self, role_name: str, old_org: Any) -> None:
        """Retire a role: release the seat here, then delete its inbox.

        Two halves with different scopes. **Releasing the seat** is local
        and only this node can do it — through the ordinary voluntary
        path, so the teardown sequence is the same one every other
        release uses. **Deleting the subscription** is fleet-wide and
        must happen exactly once, after the seat is released and no node
        will re-claim it; it goes through the admin API, which needs no
        local consumer, so it does not depend on which node ran the seat.

        Safe for human seats and never-spawned roles.
        """
        from crewlet.seat.host import ReleaseReason

        old_role = old_org.get_role(role_name)
        handle = old_role.get_handle() if old_role is not None else ""

        if handle and self._seat_host is not None:
            # Bounded drain first (the role still exists for a moment
            # longer), then the standard release.
            await self._drain_seat(role_name)
            await self._seat_host.release(handle, ReleaseReason.ROLE_GONE)
        else:
            await self._drain_seat(role_name)
            for agent in self.agent_pool.get_all_for_role(role_name):
                for issue_key in self.execution_tracker.get_issues(agent.id_str):
                    self.execution_tracker.untrack(issue_key)
                self._subscribed_inboxes.pop(agent.handle, None)
                await self.agent_pool.terminate(agent)
                logger.info("agent_terminated", agent_id=agent.id_str, role=role_name)

        if handle:
            # Destructive, and deliberately so: a decommissioned seat's
            # inbox must not accumulate undeliverable events forever.
            for topic, group in (
                (agent_inbox_topic(handle), agent_inbox_group(handle)),
                (agent_control_topic(handle), agent_control_group(handle)),
            ):
                try:
                    await self.event_queue.delete_subscription(topic, group)
                except Exception:
                    logger.exception("inbox_unsubscribe_failed", handle=handle)
            self._subscribed_inboxes.pop(handle, None)
            # Keyed on the handle, not on a live instance: under seat
            # ownership the release above already terminated it, and on a
            # node that never held the seat there was never one to ask.
            self._purge_cli_llm_workspaces(handle)

        # Release the seat's Mattermost bot: its websocket, its HTTP
        # client and its handle-registry entries.  Unlike every other
        # transport, Mattermost holds a LIVE outbound connection per
        # seat — leaving it open keeps a departed handle authenticated
        # with a personal access token, publishing inbound events the
        # notification service can no longer route, and reconnecting
        # forever.
        await self._unregister_role_mattermost_bot(role_name, old_org)

        # Stop the role's per-role MCP subprocesses (atlassian / slack
        # / github) and drop its cached tool list.  Terminating the
        # agent alone leaks the running MCP clients and leaves a stale
        # ``_role_mcp_tools`` entry that a later role reusing the name
        # would surface.  Guard on the spawn cascade — before it runs
        # there are no per-role instances to stop.
        if self._tier_b_done and old_role is not None:
            await self._stop_role_mcp(old_role)
        self._role_mcp_tools.pop(role_name, None)

    async def _unregister_role_mattermost_bot(
        self, role_name: str, old_org: Any
    ) -> None:
        """Drop a departing role's Mattermost bot from the transport."""
        transport = self._get_running_mattermost_transport()
        if transport is None:
            return
        old_role = old_org.get_role(role_name) if old_org is not None else None
        if old_role is None or not getattr(old_role, "mattermost", None):
            return
        try:
            await transport.unregister_bot(old_role.get_handle())
        except Exception as exc:
            logger.error(
                "mattermost_bot_unregister_failed", role=role_name, error=str(exc)
            )

    def _purge_cli_llm_workspaces(self, handle: str) -> None:
        """Delete a departing seat's CLI-agent homes.

        A ``cli-agent`` LLM provider keeps a coding CLI's home per seat
        on the engine host — including a copy of the subscription
        credential it refreshed. When the seat is decommissioned that
        directory has no owner; leaving it behind keeps a live
        credential and the seat's last prompts on disk indefinitely, and
        would be silently re-used if the handle were ever recycled.
        Best-effort: a failure here must never block the teardown.
        """
        if not handle:
            return
        for key, provider in (self._llm_providers or {}).items():
            workspace = getattr(provider, "workspace", None)
            purge = getattr(workspace, "purge_seat", None)
            if purge is None:
                continue
            try:
                purge(handle)
                logger.debug("cli_llm_workspace_purged", provider=key, handle=handle)
            except Exception:
                logger.exception(
                    "cli_llm_workspace_purge_failed", provider=key, handle=handle
                )

    async def _apply_org_diff(self, old_org: Any, new_org: Any) -> None:
        """Apply role-level differences between two :class:`Organization` s.

        Spawn new agent seats, terminate removed ones, swap
        :class:`AgentDefinition` for changed ones, and handle seat-kind
        flips (``human → agent`` spawns; ``agent → human``
        decommissions — the deterministic agent id means a later flip
        back reattaches the old diary / onboarding markers).  Human
        seats never spawn; their changes ride the org swap.
        ``apply_config`` adds per-subsystem diff handlers for the
        remaining Tier B surfaces (LLM providers, MCP servers,
        integrations, transports, turn engine, learning, extensions,
        budgets).
        """
        from crewlet.agent.definition import AgentDefinition
        from crewlet.events.types import RoleUpdated
        from crewlet.org.models import RoleKind

        current_role_names = {r.name for r in old_org.all_roles()}
        new_role_names = {r.name for r in new_org.all_roles()}

        added = new_role_names - current_role_names
        removed = current_role_names - new_role_names
        if added:
            logger.debug("new_roles_detected", roles=list(added))
        if removed:
            logger.debug("removed_roles_detected", roles=list(removed))

        # Spawn agents for new agent seats via the same AgentPool path
        # the boot cascade uses; the seed-budget + inbox-subscribe
        # steps are not pool concerns and stay in the helper.
        for role_name in added:
            role = new_org.get_role(role_name)
            if role is None:
                continue
            if role.kind == RoleKind.HUMAN:
                # Nothing to spawn — the seat becomes visible through
                # the org swap; contact IDs register after the swap.
                logger.info("human_seat_added", role=role_name)
                continue
            await self._spawn_role_live(role, new_org)

        # Terminate agents for removed roles
        for role_name in removed:
            await self._decommission_role_live(role_name, old_org)

        # Detect and apply property changes for existing roles
        from crewlet.org.models import Role

        role_identity_fields = {"name"}
        compare_fields = frozenset(Role.model_fields) - role_identity_fields

        updated_count = 0
        # Seats that flipped human → agent spawn from the kept set, not
        # ``added`` — track them so the post-swap Jira/GitHub identity
        # refresh covers them too (otherwise the new agent's external
        # IDs never register until a restart or an unrelated role add).
        flipped_to_agent: set[str] = set()
        for role_name in current_role_names & new_role_names:
            old_role = old_org.get_role(role_name)
            new_role = new_org.get_role(role_name)
            if old_role is None or new_role is None:
                continue

            if old_role.kind != new_role.kind:
                # Seat-kind flip: same name, different holder.
                updated_count += 1
                if new_role.kind == RoleKind.HUMAN:
                    logger.info("seat_became_human", role=role_name)
                    await self._decommission_role_live(role_name, old_org)
                else:
                    logger.info("seat_became_agent", role=role_name)
                    await self._spawn_role_live(new_role, new_org)
                    flipped_to_agent.add(role_name)
                continue

            changed = [
                f
                for f in compare_fields
                if getattr(old_role, f) != getattr(new_role, f)
            ]
            if not changed:
                continue

            updated_count += 1
            logger.debug("role_updated", role=role_name, changed_fields=changed)

            # Let this seat's in-flight turns finish before swapping what
            # they are running as.  A turn already pins its definition,
            # providers and tool catalogue (see crewlet.agent.turn_pin),
            # so it stays *coherent* through a rewire — but pinning a
            # catalogue is not pinning a capability, and the MCP respawn
            # below genuinely kills the clients a running turn's tools
            # dispatch to.  Bounded, and never on ``AgentState``: a seat
            # parked on a detached sandbox run would otherwise hold the
            # whole apply for the length of the run.
            await self._drain_seat(role_name)

            if new_role.kind == RoleKind.HUMAN:
                # Human seats have no AgentDefinition or instance —
                # the org swap below carries contact / availability /
                # notify / hierarchy changes everywhere they render.
                continue

            # Build one definition per role, share across agents
            new_defn = AgentDefinition(
                role=new_role,
                org=new_org,
            )
            self.agent_pool._definitions[role_name] = new_defn

            agents = self.agent_pool.get_all_for_role(role_name)
            for agent in agents:
                agent.definition = new_defn

                # A changed ``token_budget`` is applied by the
                # ``_reseed_seat_budgets`` pass after the org swap, which
                # covers seats this node does not run as well.
                await self.event_queue.publish(
                    "crewlet.events.role_updated",
                    RoleUpdated(
                        source="engine.apply_config",
                        role=role_name,
                        agent_id=agent.id_str,
                        changed_fields=changed,
                    ),
                )

            # When a role's per-role MCP env changed, the running
            # per-role MCP processes baked in the prior values (env vars /
            # http headers) and won't see the new ones until restarted —
            # this covers every per-agent tool credential, since they all
            # live in ``mcp_env`` now (Atlassian / GitHub / the Slack MCP
            # token).  ``slack`` (the transport identity) is included
            # because a bot-token change there usually accompanies the
            # matching ``mcp_env.slack`` rotation.  Only fire after the
            # spawn cascade has run — before then, ``_start_mcp_servers``
            # spawns them fresh.
            if self._tier_b_done and ("mcp_env" in changed or "slack" in changed):
                await self._respawn_role_mcp(new_role)

        # Swap org after all agents are updated to avoid partial state
        self.org = new_org

        # Per-seat token caps are a projection of the active org, so
        # reconcile them against it once, here — rather than per spawned
        # instance, which skipped every seat this node does not run.
        self._reseed_seat_budgets(self.org)

        # Placement reconciles against the NEW org: added seats become
        # claimable, removed ones are released. It has to run after the
        # swap because the host reads its seat list from ``self.org``.
        if self._tier_b_done:
            if self._seat_host is None:
                self._seat_host = self._build_seat_host()
            await self._ensure_seat_subscriptions()
            with contextlib.suppress(Exception):
                await self._seat_host.sweep()

        # Org config reaches agents directly through the
        # in-memory Organization (rendered into the Plan prompt by
        # the section builders in agent.definition); no knowledge
        # store reseed needed when the config reloads.

        # Human contact IDs are declared in config (no MCP), so refresh
        # them on every org swap — covers added humans, kind flips, and
        # contact edits regardless of the ``added``-gated refresh below.
        if self.handle_registry is not None:
            from crewlet.notifications.handle import register_human_contacts_from_org

            register_human_contacts_from_org(self.handle_registry, self.org)

        # The transports' fall-through routing maps (Jira project /
        # Confluence space / Plane project → unit lead) are built from
        # the org, so an org-only edit — a unit's ``integrations.*``
        # identity or its lead — must re-seed the RUNNING transports
        # too; the integrations diff only re-seeds when a transport is
        # rebuilt, which an org edit never triggers.
        self._reseed_notification_routing()

        # Now that ``self.org`` carries the just-spawned roles, refresh
        # the registries that read from it (Jira accountId, GitHub
        # username) — covering both genuinely-added roles and seats
        # that flipped human → agent.  Slack apps were registered
        # per-role inline above because ``register_slack_apps_from_org``
        # walks ``self.org``.  Pass only the just-spawned role names so
        # each ``POST /config/roles`` resolves one identity per new
        # agent instead of re-resolving the whole org every time
        # (O(n²) over a per-entity bootstrap).
        spawned_names = added | flipped_to_agent
        if spawned_names and self._tier_b_done:
            await self._refresh_role_external_handles(only_roles=spawned_names)

        logger.info(
            "org_diff_applied",
            added=len(added),
            removed=len(removed),
            updated=updated_count,
        )


class _SeatOwnerView:
    """Live view of the engine's placement host, for the in-turn fence.

    The turn engine is built before or after the host depending on
    whether the company had LLM providers at boot, so it cannot capture
    the host by value. This reads it per call and answers "may this seat
    keep working?" — which is ``None`` both when the host does not exist
    (single-node embeds, tests) and when the seat is not ours.
    """

    def __init__(self, engine: Engine) -> None:
        self._engine = engine

    def may_start(self, handle: str) -> int | None:
        host = self._engine._seat_host
        if host is None:
            # No placement at all: nothing can take the seat away, so
            # there is nothing to fence against.
            return 1
        return host.may_start(handle)
