"""TurnEngine — the Plan/Execute/Review orchestrator.

Entry point for every agent turn in Crewlet. Owns:

- concurrency (one ``ConcurrencyController`` slot per turn, fairness FIFO),
- OTel context restoration -- called exactly once per turn from the
  trigger event; the three phase spans live as children of the single
  ``agent.turn`` span,
- lifecycle: ``start_working`` / ``finish_working`` and
  ``TaskStarted`` / ``TaskCompleted`` / ``TaskFailed`` publishes,
- phase dispatch with the ``max_iterations`` cap and stall detection,
- the final ``AgentTurnCompleted`` event carrying per-phase model
  keys, iteration count, and sub-agent totals.

Runtime invariants that cross phase boundaries (depth cap, stall
detection) live in :mod:`crewlet.agent.guards`.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any
from uuid import UUID

import structlog

from crewlet._logging import get_logger
from crewlet.agent.execute import ExecuteResult, run_execute_phase
from crewlet.agent.guards import (
    DelegationDepthExceeded,
    StallDetector,
    check_delegation_depth,
)
from crewlet.agent.instance import AgentInstance, AgentState
from crewlet.agent.iteration_log import IterationRecord
from crewlet.agent.llm_loop import describe_failure, phase_failure_guard
from crewlet.agent.phase_model import resolve_phase_chain, resolve_phase_provider
from crewlet.agent.plan import (
    PLAN_META_TOOL_NAMES,
    ExecutionPlan,
    fetch_post_plan_relevant_knowledge,
    run_plan_phase,
)
from crewlet.agent.review import run_review_phase
from crewlet.agent.turn_context import TurnContext
from crewlet.agent.turn_pin import TurnPin, pinned
from crewlet.db.token_usage import TokenUsageRepository
from crewlet.events.types import (
    AgentTurnCompleted,
    EpisodeWritten,
    Event,
    LLMUnavailable,
    TaskCompleted,
    TaskFailed,
    TaskStarted,
    TurnCompleted,
    TurnGuardBreach,
    describe_trigger,
)
from crewlet.learning.episode_store import EpisodeStoreProtocol
from crewlet.learning.models import Episode
from crewlet.providers.fallback import FallbackLLMProvider, LLMChainExhausted
from crewlet.providers.llm.protocol import LLMProvider
from crewlet.providers.llm.scope import bind_llm_scope
from crewlet.queue.protocol import EventQueue
from crewlet.telemetry import restore_context, set_span_error, tracer
from crewlet.tools.capabilities import resolve_annotations
from crewlet.tools.protocol import AgentContext, CheckContext, ToolResultValidator
from crewlet.tools.registry import ToolRegistry, build_availability_set
from crewlet.tools.surface import ToolSurface
from crewlet.work_key import current_work_key

logger = get_logger("agent.turn")

# How long a config apply waits for one seat's in-flight turns to finish
# before mutating that seat.
#
# The thing being waited on is the tail of an LLM round — the unit of
# work a turn cannot be interrupted inside, and the reason a mid-turn
# rewire is visible at all.  Ten seconds comfortably covers a completion
# from every provider Crewlet ships against, and a turn that is between
# rounds releases its count immediately, so in practice the wait is
# either ~0 or one round.
#
# It is a CAP, not a contract. Past it the apply proceeds and logs
# ``seat_drain_timed_out``: an apply that blocks indefinitely on one busy
# seat is strictly worse than one turn seeing a mid-flight rewire — which
# is what every turn saw before the drain existed, and which the turn pin
# already makes survivable.
SEAT_DRAIN_TIMEOUT_SECONDS = 10.0


def _annotation_is_read(ann: Any) -> bool:
    """True when a :class:`ToolAnnotations` positively marks a read."""
    return ann is not None and getattr(ann, "read_only", None) is True


def _phase_delivered(
    called: set[str],
    real_tools_needed: set[str],
    mcp_tool_names: set[str],
    known_read_names: set[str],
) -> bool:
    """Did a phase deliver the action the plan implies?

    Name-precise when the planner named tools that resolve in the
    catalogue (``real_tools_needed``): the exact tool must have been
    called.  When the planner named ONLY phantom guesses — MCP tool
    names it can't see and got wrong — we can't name-match, so fall
    back: delivered iff the phase called a **server-backed MCP tool**
    (``mcp_tool_names``) that isn't a positively-known read.  That is
    the real delivery tool the phase discovered, whatever its name.

    Requiring a server-backed tool is the point: a delivery to a shared
    surface only ever comes from an MCP server, so a first-party builtin
    call (``lookup_colleague``, ``reflect_and_persist``, …) a phase made
    during recon never counts, and neither does an explicit read.

    This preserves the double-post fix (a real delivery via the
    discovered MCP tool reads as delivered, so the ``done`` →
    ``self_iterate`` override doesn't fire and re-post) while catching
    the bug it left open: a plan whose only delivery tool was a wrong
    guess and that called no real delivery tool must NOT read as
    delivered — otherwise a "reply hi" that produced text but never
    called Slack would complete silently without ever acting, even one
    that called a builtin like ``lookup_colleague`` first.
    """
    if real_tools_needed:
        return bool(called & real_tools_needed)
    return any(
        name in mcp_tool_names and name not in known_read_names for name in called
    )


async def _set_typing_phase(session: Any, phase: str) -> None:
    """Move a Slack working-status session to *phase*, if there is one.

    Best-effort by construction: the indicator is cosmetic and must never
    break a turn, and most turns have no session at all (non-Slack
    trigger, or the agent wasn't addressed).
    """
    if session is None:
        return
    try:
        await session.set_phase(phase)
    except Exception:
        logger.exception("working_status_phase_failed", phase=phase)


async def _end_typing(session: Any, *, keep_alive: bool = False) -> None:
    """Release a Slack working-status session, if there is one."""
    if session is None:
        return
    try:
        await session.end(keep_alive=keep_alive)
    except Exception:
        logger.exception("working_status_end_failed")


def _settle_agent_after_turn(turn: Any, agent: Any) -> None:
    """Return the agent to a resting state when its turn ends.

    A turn that SUSPENDED for a detached sandbox run goes to
    ``AWAITING_SANDBOX`` synchronously — the state never passes through
    ``IDLE``, so a queued event cannot slip a turn in between the suspend
    and the coordinator processing the ``SandboxRunStarted`` event. Every
    other outcome frees the agent.

    A function rather than four inline lines because it runs from a
    ``finally`` that must not be able to skip it: an agent left
    ``WORKING`` never takes another turn for the life of the process, and
    nothing reaps that state.
    """
    if getattr(turn.last_execute_result, "status", "") == "detached":
        agent.await_sandbox(turn.task_id)
    else:
        agent.finish_working()


class SeatLost(RuntimeError):
    """A turn was abandoned because this node stopped owning the seat.

    Unlike :class:`ShutdownDraining` this can fire mid-turn, after work
    has happened: rounds have run, tools have fired, side effects may
    already be outside any transaction. Raising it does not undo those —
    nothing can — it stops the turn from producing MORE of them beside
    the seat's new owner.

    That is the honest property this whole mechanism buys: **bounded
    duplication**, not none. Epoch-fenced writes protect database state;
    the fence in the loop bounds how far a zombie gets before it
    notices. ``run_sandbox`` makes the limit vivid — it acquires a real,
    billed E2B box before the pending row is written, so no fence of any
    kind can recall a box already pushing commits.
    """


class ShutdownDraining(RuntimeError):
    """A turn was refused because the engine is draining for shutdown.

    Raised before the turn has done any work (no LLM call, no side
    effects), so the queue handler can let it propagate: the broker
    NAKs the trigger message and redelivers it to the next engine
    subscription, where a fresh turn runs from scratch.  Turns that
    were already past the concurrency gate when the drain began are
    NOT interrupted -- the engine's drain waits for them to finish.
    """


class TurnEngine:
    """Orchestrates the Plan/Execute/Review turn for a single agent."""

    def __init__(
        self,
        *,
        llm_providers: dict[str, LLMProvider],
        tool_registry: ToolRegistry,
        event_queue: EventQueue,
        role_mcp_tools: dict[str, list[Any]] | None = None,
        # Live-reload cell holding the active TurnEngineConfig.  When
        # ``None``, a default cell is constructed from the scalar
        # per-setting kwargs below (so tests that pass
        # ``max_iterations=5`` keep working without a settings cell).
        # Per-setting ``@property`` accessors read through this on
        # every access so ``Engine.apply_config`` swaps take effect on
        # the next turn.
        settings: Any = None,
        # Scalar per-setting kwargs — wrapped into a fresh
        # ``TurnEngineSettings`` when ``settings`` is not supplied.
        # A convenience for the many test sites that construct
        # ``TurnEngine`` with scalar kwargs directly.
        max_iterations: int = 3,
        subagent_max_turns: int = 20,
        subagent_timeout_seconds: float = 120.0,
        subagent_budget_fraction: float = 0.2,
        subagent_max_parallel: int = 3,
        subagent_batch_timeout_seconds: float = 120.0,
        subagent_min_per_child_tokens: int = 500,
        executor_always_on_tools: list[str] | None = None,
        delegation_depth_limit: int = 3,
        max_tool_rounds: int = 20,
        plan_max_tool_rounds: int = 16,
        onboarding_max_tool_rounds: int = 10,
        extension_enabled: bool = True,
        plan_max_tool_rounds_ceiling: int = 32,
        execute_max_tool_rounds_ceiling: int = 40,
        onboarding_max_tool_rounds_ceiling: int = 20,
        extension_round_step: int = 8,
        # Engine-wide dependencies.
        storage: Any = None,
        knowledge_searcher: Any = None,
        concurrency: Any = None,
        budget_manager: Any = None,
        observability: Any = None,
        notification_service: Any = None,
        a2a_service: Any = None,
        handle_registry: Any = None,
        execution_tracker: Any = None,
        token_usage_repo: TokenUsageRepository | None = None,
        episode_store: EpisodeStoreProtocol | None = None,
        counterparty_store: Any = None,
        synthesized_skill_store: Any = None,
        agent_diary: Any = None,
        onboarding_marker_store: Any = None,
        episode_recall_summarize: bool = True,
        episode_recall_summarize_max_tokens: int = 400,
        prompt_skill_registry: Any = None,
        sandbox_manager: Any = None,
        sandbox_pending_store: Any = None,
        sandbox_mcp_servers: Any = None,
        sandbox_otel_receiver: Any = None,
        llm_provider_configs: dict[str, Any] | None = None,
        seat_owner: Any = None,
    ) -> None:
        from crewlet.agent.turn_settings import TurnEngineSettings
        from crewlet.config import TurnEngineConfig

        # Held by reference so the engine's in-place provider swap
        # (``clear()`` + ``update()``, identity deliberately preserved) is
        # visible here.  Read through the ``_llm_providers`` property
        # below, which prefers a turn's pin — see crewlet.agent.turn_pin.
        self._llm_providers_live = llm_providers
        # Placement host, or ``None``. Read through ``_fence_for`` — the
        # turn loop asks it before every round and before every
        # side-effecting tool, so a node whose lease moved stops within
        # one round rather than running a whole turn beside its
        # successor.
        self._seat_owner = seat_owner
        self._tool_registry = tool_registry
        self._event_queue = event_queue
        # Hold the engine's dict by reference -- NOT ``or {}``, which
        # swaps in a fresh dict when the passed-in one is empty (an
        # empty dict is falsy).  The engine mutates
        # ``self._role_mcp_tools`` in place as roles are added live;
        # if the TurnEngine captured a separate empty dict at build
        # time (per-entity bootstrap builds the engine before any
        # roles exist), those later per-role MCP tools would never be
        # visible at turn time and ``list_mcp_server_tools`` would
        # report ``(none)``.
        self._role_mcp_tools: dict[str, list[Any]] = (
            role_mcp_tools if role_mcp_tools is not None else {}
        )

        # All TurnEngineConfig-derived scalars resolve through this cell
        # on every access — see the ``@property`` accessors below.
        # ``load_tool_skill`` is the only always-on tool by default: a
        # cheap read-only lookup the executor frequently wants mid-task
        # (the rich body of a Tool Skill whose catalogue summary wasn't
        # enough) and can't get later if the planner forgot to name it
        # in ``tools_needed``.  Knowledge-base search is an MCP tool
        # (e.g. ``confluence_search``) and so cannot be force-added here;
        # the planner names it in ``tools_needed`` when Execute will
        # need it.
        if settings is None:
            settings = TurnEngineSettings(
                TurnEngineConfig(
                    max_iterations=max_iterations,
                    subagent_max_turns=subagent_max_turns,
                    subagent_timeout_seconds=subagent_timeout_seconds,
                    subagent_budget_fraction=subagent_budget_fraction,
                    subagent_max_parallel=subagent_max_parallel,
                    subagent_batch_timeout_seconds=subagent_batch_timeout_seconds,
                    subagent_min_per_child_tokens=subagent_min_per_child_tokens,
                    executor_always_on_tools=(
                        list(executor_always_on_tools)
                        if executor_always_on_tools is not None
                        else ["load_tool_skill"]
                    ),
                    delegation_depth_limit=delegation_depth_limit,
                    max_tool_rounds=max_tool_rounds,
                    plan_max_tool_rounds=plan_max_tool_rounds,
                    onboarding_max_tool_rounds=onboarding_max_tool_rounds,
                    extension_enabled=extension_enabled,
                    plan_max_tool_rounds_ceiling=plan_max_tool_rounds_ceiling,
                    execute_max_tool_rounds_ceiling=execute_max_tool_rounds_ceiling,
                    onboarding_max_tool_rounds_ceiling=(
                        onboarding_max_tool_rounds_ceiling
                    ),
                    extension_round_step=extension_round_step,
                )
            )
        self._settings: TurnEngineSettings = settings

        self._storage = storage
        self._knowledge_searcher = knowledge_searcher
        self._concurrency = concurrency
        self._budget_manager = budget_manager
        self._observability = observability
        self._notification_service = notification_service
        self._a2a_service = a2a_service
        self._handle_registry = handle_registry
        self._execution_tracker = execution_tracker
        self._token_usage_repo = token_usage_repo
        self._episode_store = episode_store
        self._counterparty_store = counterparty_store
        self._synthesized_skill_store = synthesized_skill_store
        self._agent_diary = agent_diary
        self._onboarding_marker_store = onboarding_marker_store
        self._episode_recall_summarize = bool(episode_recall_summarize)
        self._episode_recall_summarize_max_tokens = int(
            episode_recall_summarize_max_tokens
        )
        # Engine-wide PromptSkillRegistry. ``None`` is supported so
        # test paths (no engine) still construct a working TurnEngine.
        self._prompt_skill_registry = prompt_skill_registry

        # Sandboxed Execute backend.  ``None`` when no
        # ``providers.sandbox`` is configured -- dispatch then never
        # selects the sandbox backend regardless of plan/role.  The
        # per-key ``LLMProviderConfig`` map lets the sandbox backend
        # derive the coding agent's creds from the role's resolved
        # provider; empty on test paths.  Held by
        # reference (NOT copied) so the engine's live ``providers.llm``
        # diff -- which mutates the dict in place -- is visible here,
        # exactly like ``self._llm_providers``.
        self._sandbox_manager = sandbox_manager
        # The detached kick-off persists a pending_sandbox_run row; without
        # a DB-backed store the engine wires a process-local memory store so
        # the always-detached path still works in tests / single-process dev.
        if sandbox_pending_store is None and sandbox_manager is not None:
            from crewlet.sandbox.pending_store import MemoryPendingSandboxRunStore

            sandbox_pending_store = MemoryPendingSandboxRunStore()
        self._sandbox_pending_store = sandbox_pending_store
        # Parsed MCPServerConfig list, so the sandbox backend can render the
        # role's scoped MCP surface into the coding agent. Empty on
        # test paths (no in-sandbox MCP, just the ask shim + GitHub).
        self._sandbox_mcp_servers = sandbox_mcp_servers or []
        # Engine-fronted OTLP receiver; ``None`` falls back to
        # a directly-configured collector endpoint (or no telemetry).
        self._sandbox_otel_receiver = sandbox_otel_receiver
        self._llm_provider_configs: dict[str, Any] = (
            llm_provider_configs if llm_provider_configs is not None else {}
        )

        self._result_validators: list[ToolResultValidator] = []

        # Graceful-shutdown gate.  Set by :meth:`begin_shutdown`; turns
        # that have not yet passed the concurrency gate abort with
        # :class:`ShutdownDraining` (NAK → redelivery on the next boot)
        # instead of starting a full Plan/Execute/Review run during the
        # drain.
        self._shutdown_event = asyncio.Event()

        # Per-seat in-flight turn count, and a broadcast that fires
        # whenever one reaches zero.  This is what :meth:`drain_seat`
        # waits on, and it is deliberately a COUNTER rather than a read
        # of ``AgentState``: a seat parked on a detached sandbox run
        # stays ``AWAITING_SANDBOX`` for the whole run plus up to a
        # clarification pause, so draining on the state would let one
        # agent's pending question block a config apply — and, through
        # it, the whole node.  A suspended turn returns from
        # ``_run_turn_inner`` and so releases its count; the resume takes
        # a fresh one.
        self._seat_in_flight: dict[str, int] = {}
        self._seat_idle: asyncio.Event = asyncio.Event()
        self._seat_idle.set()

    # ----- pinned reads ------------------------------------------------

    @property
    def _llm_providers(self) -> dict[str, Any]:
        """The provider map — a turn's pinned copy where one is in force.

        The engine's provider diff rebuilds clients and swaps them into
        this dict in place, so without the pin a turn could resolve Plan
        against one client and Execute against its replacement.
        """
        from crewlet.agent.turn_pin import current_pin

        pin = current_pin()
        if pin is not None and pin.owner == id(self):
            return pin.llm_providers
        return self._llm_providers_live

    def _role_mcp_for(self, role_name: str, agent_id: str) -> list[Any]:
        """This role's per-role MCP tools, pinned for the turn.

        Read twice per turn from two different places (the availability
        set and the tool catalogue), so an un-pinned read can produce a
        turn whose planner saw a tool its executor cannot name.
        """
        from crewlet.agent.turn_pin import pin_for

        pin = pin_for(self, agent_id)
        if pin is not None:
            return pin.role_mcp_tools
        return self._role_mcp_tools.get(role_name, [])

    def _capture_pin(self, agent: AgentInstance) -> TurnPin:
        """Snapshot everything a live apply can move out from under a turn.

        Shallow copies on purpose: the objects themselves (LLM clients,
        MCP tool wrappers) are shared with the live engine and can still
        be stopped underneath the turn.  What is pinned is the *set* —
        which providers exist, which tools this role has, which
        definition it is running as, and what the limits are — because
        that is what has to stay consistent across phases.  See
        :mod:`crewlet.agent.turn_pin` on why pinning a catalogue is not
        pinning a capability.
        """
        return TurnPin(
            owner=id(self),
            agent_id=agent.id_str,
            settings=self._settings.live(),
            llm_providers=dict(self._llm_providers_live),
            role_mcp_tools=list(self._role_mcp_tools.get(agent.role_name, [])),
            definition=agent.live_definition,
        )

    # ----- per-seat drain ----------------------------------------------

    def _seat_enter(self, role_name: str) -> None:
        self._seat_in_flight[role_name] = self._seat_in_flight.get(role_name, 0) + 1
        self._seat_idle.clear()

    def _seat_exit(self, role_name: str) -> None:
        remaining = self._seat_in_flight.get(role_name, 1) - 1
        if remaining > 0:
            self._seat_in_flight[role_name] = remaining
            return
        self._seat_in_flight.pop(role_name, None)
        # One broadcast for all waiters; each re-checks its own seat.
        # Swap-then-set would be needed for a per-seat event, but a
        # single shared event with a re-check loop is simpler and the
        # wait is bounded anyway.
        self._seat_idle.set()

    def seat_in_flight(self, role_name: str) -> int:
        """Turns currently running for ``role_name`` in this process."""
        return self._seat_in_flight.get(role_name, 0)

    def _fence_for(self, handle: str) -> Callable[[], None] | None:
        """A zero-arg fence for one seat, or ``None`` when unfenced.

        ``None`` for a single-node embed with no placement host and for
        an empty handle — there is no seat to lose.
        """
        owner = self._seat_owner
        if owner is None or not handle:
            return None

        def _check() -> None:
            if owner.may_start(handle) is None:
                raise SeatLost(
                    f"this node no longer owns seat {handle!r}; abandoning "
                    f"the turn rather than running it beside the seat's "
                    f"new owner"
                )

        return _check

    async def drain_seat(
        self, role_name: str, *, timeout: float = SEAT_DRAIN_TIMEOUT_SECONDS
    ) -> bool:
        """Wait for ``role_name``'s in-flight turns to finish.

        Returns whether the seat actually reached idle.  Called by the
        config-apply path before it mutates a seat — swapping its
        definition, respawning its per-role MCP children, terminating it
        — so the mutation lands between turns rather than inside one.
        This is what makes the turn pin load-bearing: the pin keeps a
        turn *consistent*, and draining is what keeps its tools *alive*.

        The timeout is a cap, not a contract.  What is being waited on is
        the tail of an LLM round — the unit of work a turn cannot be
        interrupted inside — and ten seconds comfortably covers one.
        Beyond that the apply proceeds anyway and logs it: an apply that
        blocks indefinitely on one busy seat is strictly worse than one
        turn seeing a mid-flight rewire, and the turn was already
        surviving that before the drain existed.
        """
        loop = asyncio.get_running_loop()
        deadline = loop.time() + max(0.0, timeout)
        while True:
            # Clear BEFORE the re-check, with no await in between: an
            # exit that lands after this point sets the event and the
            # wait below returns immediately, and one that landed before
            # it is caught by the re-check.  Clearing after the check
            # would drop exactly the wakeup being waited for.
            self._seat_idle.clear()
            if self.seat_in_flight(role_name) == 0:
                logger.debug("seat_drained", role=role_name)
                self._seat_idle.set()
                return True
            remaining = deadline - loop.time()
            if remaining <= 0:
                logger.warning(
                    "seat_drain_timed_out",
                    role=role_name,
                    in_flight=self.seat_in_flight(role_name),
                    timeout_seconds=timeout,
                )
                return False
            with contextlib.suppress(TimeoutError):
                await asyncio.wait_for(self._seat_idle.wait(), timeout=remaining)

    # ----- live-reload settings accessors -----------------------------
    # Each property reads through ``self._settings.get()`` on every
    # access so a ``Engine.apply_config`` swap takes effect on the
    # next turn without rewiring call sites.  Existing read sites in
    # this module (lines 499, 645, 814, 1193+) continue to use the
    # same ``self._<name>`` attribute syntax.

    @property
    def _max_iterations(self) -> int:
        return self._settings.get().max_iterations

    @property
    def _subagent_max_turns(self) -> int:
        return self._settings.get().subagent_max_turns

    @property
    def _subagent_timeout_seconds(self) -> float:
        return self._settings.get().subagent_timeout_seconds

    @property
    def _subagent_budget_fraction(self) -> float:
        return self._settings.get().subagent_budget_fraction

    @property
    def _subagent_max_parallel(self) -> int:
        return self._settings.get().subagent_max_parallel

    @property
    def _subagent_batch_timeout_seconds(self) -> float:
        return self._settings.get().subagent_batch_timeout_seconds

    @property
    def _subagent_min_per_child_tokens(self) -> int:
        return self._settings.get().subagent_min_per_child_tokens

    @property
    def _always_on(self) -> list[str]:
        # ``executor_always_on_tools`` defaults to ``["load_tool_skill"]``
        # -- a cheap read-only lookup the executor frequently wants
        # mid-task and can't get later if the planner forgot to name it
        # in ``tools_needed``.
        return list(self._settings.get().executor_always_on_tools)

    @property
    def _delegation_depth_limit(self) -> int:
        return self._settings.get().delegation_depth_limit

    @property
    def _max_tool_rounds(self) -> int:
        return self._settings.get().max_tool_rounds

    @property
    def _plan_max_tool_rounds(self) -> int:
        return int(self._settings.get().plan_max_tool_rounds)

    @property
    def _onboarding_max_tool_rounds(self) -> int:
        return int(self._settings.get().onboarding_max_tool_rounds)

    @property
    def _extension_enabled(self) -> bool:
        return bool(self._settings.get().extension_enabled)

    @property
    def _plan_max_tool_rounds_ceiling(self) -> int:
        return int(self._settings.get().plan_max_tool_rounds_ceiling)

    @property
    def _execute_max_tool_rounds_ceiling(self) -> int:
        return int(self._settings.get().execute_max_tool_rounds_ceiling)

    @property
    def _onboarding_max_tool_rounds_ceiling(self) -> int:
        return int(self._settings.get().onboarding_max_tool_rounds_ceiling)

    @property
    def _extension_round_step(self) -> int:
        return int(self._settings.get().extension_round_step)

    @property
    def _sandbox_budget_fraction(self) -> float:
        return float(self._settings.get().sandbox_budget_fraction)

    @property
    def _sandbox_min_budget_tokens(self) -> int:
        return int(self._settings.get().sandbox_min_budget_tokens)

    def set_sandbox_manager(self, manager: Any) -> None:
        """Swap the sandbox manager (live-reload of ``providers.sandbox``).

        The manager is a single object (not a by-reference dict like
        ``_llm_providers``), so the engine's provider diff calls this to
        publish a rebuilt manager -- or ``None`` to disable the backend --
        to the running engine without recreating it.
        """
        self._sandbox_manager = manager

    def set_sandbox_mcp_servers(self, mcp_servers: Any) -> None:
        """Swap the parsed MCP server configs (live-reload of ``mcp_servers``).

        The engine REASSIGNS ``self._mcp_configs`` on an ``mcp_servers``
        diff (a new list, not in-place), so the turn engine's captured
        snapshot would otherwise go stale; the diff calls this so the
        sandbox MCP rendering uses the current servers next turn.
        """
        self._sandbox_mcp_servers = mcp_servers or []

    def set_knowledge_searcher(self, searcher: Any) -> None:
        """Swap (or clear) the query-time knowledge searcher.

        The TurnEngine captures the searcher reference at construction
        and hands it to every turn's ``AgentContext``, so when a live
        ``integrations.confluence`` / ``integrations.plane`` edit
        rebuilds the transport -- or a cut-over swaps backends, or the
        integration is removed -- the engine's refresh path must call
        this to re-point (``searcher``) or disable (``None``) the
        ``## Relevant knowledge`` prefetch.  Without it, every turn
        would keep searching through a stopped transport and silently
        return nothing.
        """
        self._knowledge_searcher = searcher

    # ----- graceful shutdown ------------------------------------------

    def begin_shutdown(self) -> None:
        """Refuse turns that have not yet started doing real work.

        Called by ``Engine.stop()`` right after event delivery is
        paused.  Two kinds of turn exist at that moment:

        - **running** (past the concurrency gate, LLM rounds under
          way): untouched -- the engine's drain waits for them, so
          every agent finishes its current round;
        - **parked** (delivered before the pause but still waiting for
          a :class:`ConcurrencyController` slot): aborted with
          :class:`ShutdownDraining` so their trigger messages are
          NAK'd back to the broker instead of each running a full
          multi-minute turn during shutdown.
        """
        if not self._shutdown_event.is_set():
            logger.info("turn_engine_shutdown_begun")
            self._shutdown_event.set()

    @property
    def shutting_down(self) -> bool:
        """True once :meth:`begin_shutdown` has been called."""
        return self._shutdown_event.is_set()

    async def _acquire_slot_or_abort(self, agent: AgentInstance) -> None:
        """Acquire a concurrency slot, aborting if shutdown begins first.

        Races the semaphore acquire against the shutdown gate so a
        turn parked behind ``max_concurrent`` peers doesn't start a
        fresh Plan/Execute/Review run mid-drain.  Raises
        :class:`ShutdownDraining` when the gate wins; the caller rolls
        the agent back to IDLE and lets the exception NAK the message.
        """
        assert self._concurrency is not None
        acquire = asyncio.ensure_future(self._concurrency.acquire(agent.role_name))
        gate = asyncio.ensure_future(self._shutdown_event.wait())
        done, _pending = await asyncio.wait(
            {acquire, gate}, return_when=asyncio.FIRST_COMPLETED
        )
        if acquire in done:
            # Slot taken (even if the gate fired in the same tick):
            # the turn proceeds and the drain waits for it.
            gate.cancel()
            acquire.result()
            return
        acquire.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await acquire
        raise ShutdownDraining(
            f"turn for agent {agent.handle!r} aborted while waiting for a "
            "concurrency slot: engine is draining for shutdown"
        )

    async def _start_working_or_wait(self, agent: AgentInstance, task_id: str) -> None:
        """Take the agent's WORKING slot, WAITING while it is busy.

        A busy agent is normal queuing, not an error: raising
        ``RuntimeError`` would NAK the triggering event, and with the
        backends' bounded redelivery (Pulsar: 3 × 1s → dead-letter; memory:
        3 immediate retries → drop) an event arriving during a minutes-long
        turn would be effectively LOST.  So the turn parks here until the
        current turn finishes (per-agent turns stay strictly serialized) —
        the same hold-the-delivery shape the handler already has, since it
        runs the whole turn inline.  Raced against the shutdown gate so a
        parked turn NAKs cleanly on drain (redelivered to the next engine);
        a CREATED / TERMINATED agent still fails fast — that is a caller
        bug, not queuing.
        """
        waited = False
        while not agent.start_working(task_id):
            if agent.state in (AgentState.CREATED, AgentState.TERMINATED):
                raise RuntimeError(
                    f"Agent {agent.id_str} cannot start working: "
                    f"current state is {agent.state}"
                )
            if not waited:
                waited = True
                logger.info(
                    "turn_waiting_for_agent",
                    agent_id=agent.id_str,
                    agent_state=str(agent.state),
                    task_id=task_id,
                )
            change = asyncio.ensure_future(agent.wait_for_state_change())
            gate = asyncio.ensure_future(self._shutdown_event.wait())
            done, _pending = await asyncio.wait(
                {change, gate}, return_when=asyncio.FIRST_COMPLETED
            )
            for fut in (change, gate):
                if fut not in done:
                    fut.cancel()
                    with contextlib.suppress(asyncio.CancelledError):
                        await fut
            if gate in done and self._shutdown_event.is_set():
                raise ShutdownDraining(
                    f"turn for agent {agent.handle!r} aborted while waiting "
                    "for the agent to become idle: engine is draining for "
                    "shutdown"
                )

    # ----- chat working status -----------------------------------------

    async def _begin_typing_status(self, turn: TurnContext) -> Any:
        """Raise the "is thinking…" indicator for a chat-triggered turn.

        Returns a session handle, or ``None`` when there is nothing to
        drive — the trigger did not come from a chat backend, that
        backend's transport is not configured, or the mode is
        ``addressed`` and nobody addressed this agent.  Opened BEFORE the
        agent / concurrency gates so a human who pinged a busy agent sees
        feedback while the turn is still queuing, not only once it runs.

        The backend is read from the trigger's own ``transport`` metadata
        key — the same discriminator the driver re-checks — rather than
        being hardcoded, so a company running Slack and Mattermost side
        by side raises each indicator on the backend the message actually
        arrived on.

        The ``liveness`` probe is the safety net: the heartbeat drops the
        indicator within one refresh interval if the agent stops being
        busy, so a turn that dies without closing its session can't leave
        an agent looking like it is thinking forever.
        """
        service = self._notification_service
        if service is None:
            return None
        try:
            metadata = turn.notification_metadata or {}
            backend = str(metadata.get("transport") or "")
            if not backend:
                return None
            driver = getattr(service.transports.get(backend), "typing_status", None)
            if driver is None:
                return None
            agent = turn.agent
            return await driver.begin(
                handle=agent.handle,
                turn_id=turn.turn_id,
                metadata=metadata,
                liveness=lambda: (
                    agent.state in (AgentState.WORKING, AgentState.AWAITING_SANDBOX)
                ),
            )
        except Exception:
            logger.exception("working_status_begin_failed", turn_id=turn.turn_id)
            return None

    # ----- public validator registration ------------------------------

    def register_result_validator(self, validator: ToolResultValidator) -> None:
        """Register a custom tool result validator.

        This is the extension hook for redaction / sanitisation
        callbacks; the validator runs against every tool result in
        every phase (Plan / Execute / Review / sub-agent) via the
        shared :func:`~crewlet.agent.llm_loop.validate_tool_result`.
        """
        logger.debug("registering_result_validator", validator_name=validator.name)
        self._result_validators.append(validator)

    # ----- entry point ------------------------------------------------

    async def run_turn(
        self,
        agent: AgentInstance,
        *,
        task_id: str = "",
        task_description: str = "",
        org: Any = None,
        notification_metadata: dict[str, Any] | None = None,
        event: Event | None = None,
        a2a_context: dict[str, Any] | None = None,
        deadline_seconds: float | None = None,
        turn_id: str = "",
        start_iteration: int = 0,
        resume_state: Any = None,
    ) -> str:
        """Run one Plan -> Execute -> Review turn and return the final text.

        ``deadline_seconds`` sets a hard wall-clock cap on the whole turn
        (used by the scheduler); on breach the turn terminates as
        ``failed`` with a ``scheduled_timeout`` guard breach.  ``None``
        (default) runs uncapped.

        ``turn_id`` / ``start_iteration`` let a follow-on turn **continue
        under an earlier turn's id** with non-colliding iteration labels —
        used by the detached sandbox completion so its phases group with the
        kick-off turn (the dashboard groups by ``turn_id``) instead of
        appearing as a disconnected new turn. Defaults preserve the normal
        fresh-turn behaviour (a new uuid, iterations from 1).

        ``resume_state`` (an ``ExecuteResumeState``) makes this turn RESUME a
        suspended Execute tool-loop: the engine skips Plan and continues
        Execute with the persisted conversation + the sandbox result spliced
        in. Paired with ``turn_id`` + ``start_iteration``
        so the resumed phases group with the kick-off turn.
        """
        # Explicit kwargs take precedence over event fields; we fall
        # back to the event payload only when the caller didn't pass
        # a value.  Wake-up events (task_assigned, a2a_*, notification)
        # thus surface their task_id / description for free, but an
        # explicit ``task_id=`` / ``task_description=`` always wins.
        if event is not None:
            task_id = (
                task_id
                or getattr(event, "task_id", "")
                or event.payload.get("task_id", "")
            )
            task_description = (
                task_description
                or event.payload.get("task_description", "")
                or getattr(event, "body", "")
                or event.payload.get("body", "")
            )
            if org is None:
                org = event.payload.get("org")
            if notification_metadata is None:
                notification_metadata = event.payload.get("notification_metadata")
                if notification_metadata is None:
                    meta = getattr(event, "metadata", None)
                    if meta:
                        notification_metadata = meta

        # Restore OTel context from the trigger event EXACTLY ONCE.
        otel_context = restore_context(event.trace_id, event.span_id) if event else None

        # Delegation depth/chain from the event (the NotificationService
        # and A2A service copy these onto the forwarded inbox event; see
        # step 10 for the plumbing).
        delegation_depth = 0
        delegation_chain: list[str] = []
        parent_turn_id = ""
        if event is not None:
            delegation_depth = getattr(event, "delegation_depth", 0) or 0
            parent_turn_id = getattr(event, "parent_turn_id", "") or ""
            chain = getattr(event, "delegation_chain", None)
            if isinstance(chain, list):
                delegation_chain = list(chain)

        turn = TurnContext(
            agent=agent,
            org=org,
            trigger_event=event,
            task_id=task_id,
            task_description=task_description,
            parent_turn_id=parent_turn_id,
            delegation_depth=delegation_depth,
            delegation_chain=delegation_chain,
            notification_metadata=notification_metadata,
            a2a_context=a2a_context,
            deadline_seconds=deadline_seconds,
            started_at=datetime.now(UTC),
        )
        # Continue under an earlier turn's id when asked (sandbox completion),
        # so its phases group with the kick-off turn rather than starting a
        # disconnected new turn.  ``start_iteration`` offsets the iteration
        # labels so they don't collide with the kick-off's.
        if turn_id:
            turn.turn_id = turn_id
        turn.start_iteration = start_iteration
        turn.resume_state = resume_state
        # A detached sandbox run ends the turn, and the completion arrives as
        # a NEW ``run_turn`` with a fresh ``TurnContext`` — so rounds that
        # closed before the suspend must be rehydrated here or the resumed
        # turn's ledger is empty and a post-resume ``self_iterate`` re-plans
        # deliveries those rounds already made.
        if resume_state is not None:
            turn.iteration_history = [
                IterationRecord.from_dict(rec)
                for rec in getattr(resume_state, "iteration_history", []) or []
            ]

        # Three pieces of ambient turn context, all wrapping the whole
        # ``_run_turn_inner`` call rather than sitting inside it, so
        # every phase — and every sub-agent task spawned from one, which
        # inherits a copy of this context — sees the same values.
        #
        # ``bind_llm_scope`` binds the seat for every LLM call the turn
        # makes. Stateful backends — the CLI-agent provider, which keeps
        # a coding CLI's home on disk — read it to pick an isolated
        # workspace; without it a shared provider instance would let one
        # seat's CLI session leak into another's.
        #
        # ``pinned`` freezes the config this turn runs under, and the
        # seat enter/exit below counts it busy, so a live activation
        # cannot rewire a company out from under a turn in flight.
        with (
            bind_llm_scope(agent.handle or agent.role_name),
            tracer.start_as_current_span(
                "agent.turn",
                context=otel_context,
                attributes={
                    "agent.id": agent.id_str,
                    "agent.role": agent.role_name,
                    "task.id": task_id,
                    "turn.id": turn.turn_id,
                    "turn.delegation_depth": delegation_depth,
                },
            ),
            pinned(self._capture_pin(agent)),
        ):
            self._seat_enter(agent.role_name)
            try:
                return await self._run_turn_inner(turn)
            finally:
                self._seat_exit(agent.role_name)

    # ----- internals --------------------------------------------------

    async def _run_turn_inner(self, turn: TurnContext) -> str:
        agent = turn.agent
        structlog.contextvars.bind_contextvars(
            agent_name=agent.role_name,
            agent_id=agent.id_str,
        )
        logger.info("turn_starting", task_id=turn.task_id, turn_id=turn.turn_id)

        try:
            check_delegation_depth(
                depth=turn.delegation_depth, limit=self._delegation_depth_limit
            )
        except DelegationDepthExceeded as exc:
            logger.warning(
                "delegation_depth_exceeded",
                depth=turn.delegation_depth,
                limit=self._delegation_depth_limit,
            )
            await self._publish_guard_breach(
                turn=turn, kind="depth_cap", detail=str(exc)
            )
            await self._publish(
                TaskFailed(
                    source=agent.role_name,
                    role=agent.role_name,
                    task_id=turn.task_id,
                    agent_id=agent.id_str,
                    error=str(exc),
                ),
                turn=turn,
            )
            await self._publish_agent_turn_completed(
                turn,
                f"(failed: {exc})",
                "failed",
                turn_succeeded=False,
                failure=exc,
            )
            return f"(failed: {exc})"

        if self._shutdown_event.is_set():
            raise ShutdownDraining(
                f"turn for agent {agent.handle!r} refused: engine is "
                "draining for shutdown"
            )

        # Slack "is thinking…" indicator (issue: bolt-js#885).  Raised
        # here — before the agent / concurrency gates — so a human who
        # pinged a busy agent sees acknowledgement while the turn queues.
        # Every exit from here on must release it: the gates below can
        # raise before the main try/finally is entered.
        typing = await self._begin_typing_status(turn)
        try:
            await self._start_working_or_wait(agent, turn.task_id)

            # Concurrency slot: one turn per role concurrently, fairness
            # via FIFO queue in :class:`ConcurrencyController`.  Raced
            # against the shutdown gate: a turn still parked here when the
            # engine starts draining is rolled back and NAK'd instead of
            # running a full Plan/Execute/Review pass during shutdown.
            if self._concurrency is not None:
                try:
                    await self._acquire_slot_or_abort(agent)
                except ShutdownDraining:
                    agent.finish_working()
                    raise
        except BaseException:
            await _end_typing(typing)
            raise

        final_text = ""
        decision: str = "done"
        turn_succeeded = True
        timed_out = False
        # The exception that ended the turn, if any — read by the
        # ``finally`` block's ``AgentTurnCompleted`` publish so the turn
        # row a dashboard renders names its own cause instead of showing
        # an empty response.
        failure: BaseException | None = None
        try:
            await self._publish(
                TaskStarted(
                    source=agent.role_name,
                    role=agent.role_name,
                    task_id=turn.task_id,
                    agent_id=agent.id_str,
                ),
                turn=turn,
            )
            if turn.deadline_seconds is not None and turn.deadline_seconds > 0:
                # Hard wall-clock cap (scheduled turn).  ``asyncio.timeout``
                # + ``cap.expired()`` distinguishes OUR cap firing from a
                # ``TimeoutError`` raised by tool / IO code inside the turn: an
                # inner timeout (cap NOT expired) is re-raised so it follows the
                # normal ``except Exception`` failure path instead of being
                # mislabelled a wall-clock breach.
                try:
                    async with asyncio.timeout(turn.deadline_seconds) as cap:
                        final_text, decision = await self._drive_phases(turn, typing)
                except TimeoutError as exc:
                    if not cap.expired():
                        raise
                    secs = turn.deadline_seconds
                    timed_out = True
                    turn_succeeded = False
                    decision = "failed"
                    failure = exc
                    final_text = final_text or "(scheduled task timed out)"
                    set_span_error(exc)
                    logger.warning(
                        "scheduled_task_timeout",
                        turn_id=turn.turn_id,
                        agent_id=agent.id_str,
                        deadline_seconds=secs,
                    )
                    # Best-effort: a publish failure here must NOT escalate into
                    # the outer ``except Exception`` (which would double-publish
                    # the failure and re-raise, spuriously NAK-ing the scheduled
                    # task for redelivery).
                    try:
                        await self._publish_guard_breach(
                            turn=turn,
                            kind="scheduled_timeout",
                            detail=f"turn exceeded {secs}s wall-clock cap",
                        )
                        await self._publish(
                            TaskFailed(
                                source=agent.role_name,
                                role=agent.role_name,
                                task_id=turn.task_id,
                                agent_id=agent.id_str,
                                error=f"scheduled task exceeded {secs}s",
                            ),
                            turn=turn,
                        )
                    except Exception:
                        logger.exception("scheduled_timeout_publish_failed")
            else:
                final_text, decision = await self._drive_phases(turn, typing)
            if timed_out:
                pass  # the timeout path published its own TaskFailed
            elif decision == "failed":
                # A stall abort or an exhausted iteration cap ends the
                # loop by RETURNING "failed" rather than raising, so this
                # is not the exception path — but it is still a failure,
                # and publishing TaskCompleted for it recorded an
                # aborted turn in the task ledger as a success.
                breach = turn.guard_breach or {}
                await self._publish(
                    TaskFailed(
                        source=agent.role_name,
                        role=agent.role_name,
                        task_id=turn.task_id,
                        agent_id=agent.id_str,
                        error=str(breach.get("detail", "") or "turn aborted"),
                    ),
                    turn=turn,
                )
            else:
                await self._publish(
                    TaskCompleted(
                        source=agent.role_name,
                        role=agent.role_name,
                        task_id=turn.task_id,
                        agent_id=agent.id_str,
                        result=final_text[:2000],
                    ),
                    turn=turn,
                )
        except LLMChainExhausted as exc:
            # Every provider in the role's chain failed. The agent is
            # effectively AFK; publish ``LLMUnavailable`` so the
            # dashboard surfaces the cause and terminate the turn
            # cleanly as ``failed`` rather than crashing the handler.
            turn_succeeded = False
            decision = "failed"
            final_text = "(no output)"
            failure = exc
            set_span_error(exc)
            logger.error(
                "llm_unavailable",
                turn_id=turn.turn_id,
                agent_id=agent.id_str,
                provider_chain=exc.chain,
                last_error=str(exc.last_exc),
            )
            try:
                await self._publish(
                    LLMUnavailable(
                        source=agent.role_name,
                        agent_id=agent.id_str,
                        role=agent.role_name,
                        provider_chain=exc.chain,
                        attempt_count=len(exc.chain),
                        last_error_kind=exc.last_error_kind,
                        last_error=str(exc.last_exc),
                        turn_id=turn.turn_id,
                    ),
                    turn=turn,
                )
            except Exception:
                logger.exception("llm_unavailable_publish_failed")
            await self._publish(
                TaskFailed(
                    source=agent.role_name,
                    role=agent.role_name,
                    task_id=turn.task_id,
                    agent_id=agent.id_str,
                    error=f"LLM unavailable: {exc.last_exc}",
                ),
                turn=turn,
            )
        except Exception as exc:
            turn_succeeded = False
            decision = "failed"
            failure = exc
            set_span_error(exc)
            logger.exception("turn_failed", turn_id=turn.turn_id)
            try:
                await self._publish_guard_breach(
                    turn=turn,
                    kind="unhandled_exception",
                    detail=str(exc),
                )
            except Exception:
                logger.exception("unhandled_exception_breach_publish_failed")
            await self._publish(
                TaskFailed(
                    source=agent.role_name,
                    role=agent.role_name,
                    task_id=turn.task_id,
                    agent_id=agent.id_str,
                    error=str(exc),
                ),
                turn=turn,
            )
            raise
        finally:
            # Publishing the completion is reporting; releasing the slot
            # and settling the agent's state are the invariants. So the
            # reporting gets its own guard rather than standing in front
            # of them: a concurrency slot is never handed back if this
            # raises, and one that leaks is gone for the life of the
            # process — enough of them and ``acquire`` blocks forever
            # and the engine stops running turns at all. The agent would
            # be left WORKING with the same permanence.
            #
            # The publish itself is internally guarded; what this covers
            # is everything around those guards (the failure
            # description, the tracer, a future edit) plus cancellation
            # landing on the await. None of that is worth an engine.
            try:
                await self._publish_agent_turn_completed(
                    turn,
                    final_text,
                    decision,
                    turn_succeeded=turn_succeeded,
                    failure=failure,
                )
            finally:
                if self._concurrency is not None:
                    self._concurrency.release(agent.role_name)
                _settle_agent_after_turn(turn, agent)
            # A turn that SUSPENDED for a detached sandbox run transitions its
            # OWN agent to AWAITING_SANDBOX here, synchronously — the state
            # never passes through IDLE, so a queued event can't slip a turn
            # in between the suspend and the coordinator processing the
            # SandboxRunStarted event.  The coordinator's on_run_started then
            # only pauses the inbox (and handles post-restart recovery where
            # this transition never ran).  Every other outcome frees the
            # agent as before.  Done in ``_settle_agent_after_turn`` above,
            # inside the inner ``finally``; read here only to decide
            # whether the typing indicator is held across the wait.
            suspended = getattr(turn.last_execute_result, "status", "") == "detached"
            # The Slack indicator ends when the agent has replied or given
            # up.  A turn that SUSPENDED for a detached sandbox run has
            # done neither — the same turn_id resumes when the coding job
            # completes — so its session is held open across the wait.
            await _end_typing(typing, keep_alive=suspended)
            structlog.contextvars.unbind_contextvars("agent_name", "agent_id")

        return final_text

    async def _drive_phases(
        self, turn: TurnContext, typing: Any = None
    ) -> tuple[str, str]:
        """Run the Plan → Execute → Review loop.

        ``typing`` is the turn's Slack working-status session (or ``None``);
        each phase boundary swaps its text so the human sees what the agent
        is doing rather than an undifferentiated "thinking".
        """
        agent_context = self._build_agent_context(turn)
        role = turn.agent.definition.role
        role_mcp = self._role_mcp_for(turn.agent.role_name, turn.agent.id_str)

        # Resolve per-tool availability once per turn from the
        # role's mcp_env + each registered ``check_fn``.  Cached on the
        # ``TurnContext`` so the same check_fn is not invoked across
        # Plan / Execute / Review / sub-agent phases.  Tools without a
        # check_fn are always available (default-allow).
        if turn.availability_set is None:
            role_sandbox_on = role.sandbox is not None and role.sandbox.enabled
            check_ctx = CheckContext(
                agent_handle=turn.agent.handle,
                role_name=turn.agent.role_name,
                mcp_env=getattr(role, "mcp_env", {}) or {},
                sandbox_enabled=(role_sandbox_on and self._sandbox_manager is not None),
            )
            all_tool_names = [
                *(t.name for t in self._tool_registry.list_tools()),
                *(t.name for t in role_mcp),
            ]
            turn.availability_set = build_availability_set(
                self._tool_registry, check_ctx, all_tool_names
            )

        stall = StallDetector(threshold=2)
        plan_notes = ""
        final_artifact = ""
        decision: str = "done"
        execute_result: ExecuteResult | None = None
        plan: ExecutionPlan | None = None

        # First-turn onboarding runs as its OWN phase before Plan, on a
        # separate round budget, so it never competes with planning for
        # ``submit_plan`` rounds on a first turn. Skipped on a
        # resume turn (a sandbox-completion continuation, not a fresh first
        # turn) and when the agent is already onboarded. Best-effort: a failure
        # never blocks the turn — Plan still runs (and would re-show the hint).
        if turn.resume_state is None and self._onboarding_max_tool_rounds > 0:
            try:
                turn.onboarding_ran = await self._run_onboarding(
                    turn, agent_context, role, role_mcp, typing
                )
            except Exception:
                logger.exception("onboarding_phase_failed", turn_id=turn.turn_id)

        for iteration in range(self._max_iterations):
            # ``turn.start_iteration`` offsets only the iteration LABEL
            # (events / dashboard grouping) so a follow-on turn continuing
            # under an earlier turn_id doesn't collide with the kick-off's
            # iterations; the loop control + cap are unchanged.
            turn.iteration = iteration + 1 + turn.start_iteration
            logger.info(
                "turn_iteration",
                turn_id=turn.turn_id,
                iteration=turn.iteration,
                max=self._max_iterations,
            )

            # Reset the Plan-phase execution log for this iteration.
            # ``plan_tool_sequence`` (names only) stays cumulative for
            # ReflectEngine; ``plan_tool_executions`` is per-iteration
            # so Review's prompt and the delivery-override check
            # never see iter-(N-1)'s Plan calls as "delivery this
            # iteration".  Without this reset, an iter-1 Slack post
            # would satisfy iter-2's delivery check and the override
            # would silently miss a genuine iter-2 delivery gap.
            turn.plan_tool_executions = []

            judge_key, judge_provider = self._build_phase_provider(
                role, "judge", agent_handle=turn.agent.handle
            )

            # Resume of a suspended Execute loop: on the
            # first iteration of a resume turn there is no new Plan — reuse
            # the persisted plan and jump straight to Execute, which will
            # continue the saved conversation with the sandbox result.
            resume = (
                turn.resume_state
                if (iteration == 0 and turn.resume_state is not None)
                else None
            )

            if resume is not None:
                plan = resume.plan
                turn.last_plan = plan
                effective_plan = plan
                execute_relevant_knowledge = ""
            else:
                # --- Plan ---
                plan_key, plan_provider = self._build_phase_provider(
                    role, "plan", agent_handle=turn.agent.handle
                )
                await _set_typing_phase(typing, "plan")
                plan = await self._child_phase(
                    "agent.turn.plan",
                    run_plan_phase,
                    turn=turn,
                    provider=plan_provider,
                    provider_key=plan_key,
                    registry=self._tool_registry,
                    role_mcp_tools=role_mcp,
                    event_queue=self._event_queue,
                    agent_context=agent_context,
                    max_rounds=self._plan_max_tool_rounds,
                    model_split_enabled=self._model_split_enabled(role),
                    budget_manager=self._budget_manager,
                    observability=self._observability,
                    token_usage_repo=self._token_usage_repo,
                    validators=self._result_validators,
                    judge_provider=judge_provider,
                    judge_provider_key=judge_key,
                    extension_enabled=self._extension_enabled,
                    extension_ceiling=self._plan_max_tool_rounds_ceiling,
                    extension_round_step=self._extension_round_step,
                    # The personal-memory / relevant-knowledge prefetches
                    # always need the provider pool to run their aux-LLM
                    # relevance filter, so pass it unconditionally.  Only
                    # episode-recall *summarisation* is operator-toggleable
                    # -- routed via the dedicated ``episode_recall_summarize``
                    # flag, not by starving the whole prefetch layer of
                    # providers.
                    llm_providers=self._llm_providers,
                    episode_recall_summarize=self._episode_recall_summarize,
                    episode_recall_summarize_max_tokens=(
                        self._episode_recall_summarize_max_tokens
                    ),
                    prompt_skill_registry=self._prompt_skill_registry,
                )
                # Stash the latest plan so the turn-completion publisher
                # can include plan_summary in the ``TurnCompleted`` event.
                turn.last_plan = plan

                # --- Short-circuit on Plan's top-level decision ---
                # - "skip"     : nobody was actually asking the agent to act
                #                (informational / passing reference / wrong
                #                addressee).  Direct-but-declining cases must
                #                instead emit `decision="plan"` with a one-
                #                step reply -- see PLAN_HEADER in prompts.py.
                # - "direct"   : no separate planning step; let Execute use
                #                the full tool registry as its surface.
                # - "plan"     : normal Plan -> Execute -> Review path.
                # Handing off to a colleague is NOT a short-circuit: the
                # planner emits "plan" with a step that reaches the
                # colleague on the surface where the work lives (Slack
                # mention, Jira comment, a2a_ask) and Execute calls that
                # tool like any other.
                if plan.decision == "skip":
                    logger.info("plan_decision_skip", turn_id=turn.turn_id)
                    final_artifact = plan.reasoning or "(skip: not addressed to me)"
                    decision = "done"
                    break

                # Execute's tool surface is ``tools_needed ∪ always_on``
                # regardless of decision. If the planner forgot to name
                # tools, Execute gets only ``always_on`` — an unnameable
                # action tool fails fast (``execute.missing_tool``) rather
                # than silently dragging in the full catalogue.
                effective_plan = plan

                # Post-Plan relevant-knowledge re-fetch.  On a thin-trigger
                # turn the Plan-phase ``## Relevant knowledge`` prefetch was
                # gated off — there is nothing to embed-search a bare
                # pointer against.  Now that Plan has done its recon, re-run
                # the fetch keyed on the plan summary and inject the result
                # into the Execute prompt.  Returns "" on every non-thin-
                # trigger turn (the common case) and for skip decisions,
                # which never reach Execute.  Runs each iteration
                # because ``self_iterate`` produces a fresh plan summary.
                # Called directly (not via ``_child_phase``): it no-ops on
                # the common path, and wrapping every turn in a span just to
                # trace the rare thin-trigger case would litter traces with
                # empty spans — the aux-LLM call it makes carries its own
                # telemetry, and it emits ``RelevantKnowledgeRefetched``.
                execute_relevant_knowledge = await fetch_post_plan_relevant_knowledge(
                    turn=turn,
                    agent_context=agent_context,
                    plan=effective_plan,
                    llm_providers=self._llm_providers,
                )

            # --- Execute ---
            # Narrow the sub-agent config's ``parent_tool_names`` to the
            # Execute phase's actual ToolSurface for THIS iteration --
            # i.e. ``effective_plan.tools_needed ∪ always_on`` -- so
            # ``ToolSurface.for_subagent``'s "requested tools must be a
            # subset of the parent's tools" invariant matches what the
            # parent turn actually had access to.  Using the full
            # registry would let a parent LLM grant a sub-agent
            # capabilities the parent didn't have.
            exe_surface = ToolSurface.for_execute(
                self._tool_registry,
                role_mcp,
                tools_needed=effective_plan.tools_needed,
                always_on=self._always_on,
                availability_filter=turn.availability_set,
            )
            # Names the planner put in ``tools_needed`` that don't resolve
            # in Execute's catalogue.  The planner never sees MCP tool
            # *names* (its catalogue lists servers only), so these are
            # almost always wrong guesses at an MCP tool's name -- e.g.
            # ``slack_conversations_postMessage`` when the deployed Slack
            # server exposes ``slack_conversations_add_message``.  Passed
            # to Execute so it discovers the real tool instead of assuming
            # the named one exists and stopping at a text reply; reused by
            # the delivery gate after Execute runs.
            exe_catalogue = set(exe_surface.catalogue_names())
            plan_phantom_tools = sorted(set(plan.tools_needed) - exe_catalogue)
            if plan_phantom_tools:
                logger.debug(
                    "tools_needed_not_in_catalogue",
                    turn_id=turn.turn_id,
                    phantom=plan_phantom_tools,
                )
            agent_context.__dict__["spawn_subagent_config"]["parent_tool_names"] = list(
                exe_surface.names
            )

            # --- Execute (native LLM tool-loop) ---
            # Code work is the ``run_sandbox`` tool the executor calls from
            # here: it suspends the loop and returns
            # ``status="detached"``; the coordinator resumes it (``resume``)
            # when the detached run completes.
            exe_key, exe_provider = self._build_phase_provider(
                role, "execute", agent_handle=turn.agent.handle
            )
            await _set_typing_phase(typing, "execute")
            execute_result = await self._child_phase(
                "agent.turn.execute",
                run_execute_phase,
                turn=turn,
                plan=effective_plan,
                provider=exe_provider,
                provider_key=exe_key,
                registry=self._tool_registry,
                role_mcp_tools=role_mcp,
                always_on=self._always_on,
                event_queue=self._event_queue,
                agent_context=agent_context,
                max_rounds=self._max_tool_rounds,
                budget_manager=self._budget_manager,
                observability=self._observability,
                token_usage_repo=self._token_usage_repo,
                validators=self._result_validators,
                relevant_knowledge_block=execute_relevant_knowledge,
                phantom_tools=plan_phantom_tools,
                prompt_skill_registry=self._prompt_skill_registry,
                judge_provider=judge_provider,
                judge_provider_key=judge_key,
                extension_enabled=self._extension_enabled,
                extension_ceiling=self._execute_max_tool_rounds_ceiling,
                extension_round_step=self._extension_round_step,
                # run_sandbox suspends the loop; the store lets Execute persist
                # the conversation, and resume_from continues it when the
                # detached run completes.
                pending_store=self._sandbox_pending_store,
                resume_from=resume,
            )
            # Stash for the turn-completion publisher (TurnCompleted).
            turn.last_execute_result = execute_result

            # --- Detached: run_sandbox suspended the loop; end the turn ---
            # Execute called run_sandbox, which started a background coding job
            # and persisted the suspended conversation.
            # There is nothing for Review to judge yet, so the turn ends; the
            # coordinator (woken by the SandboxRunStarted the tool emitted)
            # holds the agent busy and RESUMES this Execute loop when the run
            # completes, splicing the result in.
            if execute_result.status == "detached":
                logger.info(
                    "turn_detached_sandbox_suspend",
                    turn_id=turn.turn_id,
                    sandbox_id=execute_result.sandbox_id,
                )
                turn.model_keys["review"] = ""
                final_artifact = execute_result.text or ""
                decision = "done"
                plan_notes = ""
                break

            # --- Review (mandatory on plan decisions) ---
            # Review runs after Execute on every ``plan`` decision --
            # there is deliberately no planner opt-out: recon-only
            # plans that terminate after Execute with nothing
            # delivered cost far more than the tokens Review spends.
            #
            # ``decision == "direct"`` is the one branch that still
            # skips Review: a "direct" plan, by definition, has no
            # explicit plan and therefore no success criteria for
            # Review to judge against.  Letting Review run in that
            # state invites it to hallucinate criteria from the
            # agent's role description and loop the turn -- re-
            # firing external side effects (Slack posts, Jira
            # comments) that already happened.
            skip_review = plan.decision == "direct"

            # Delivery-check sets.  Both filter out failed calls
            # (``success is False``) — a Slack post that returned 5xx
            # is NOT delivery — and the engine-side Plan set also drops
            # the Plan meta-tools (``submit_plan`` / ``activate_tool`` /
            # ``load_tool_skill``) so a hallucinated ``tools_needed``
            # entry naming one of those can't trivially satisfy the
            # check (every Plan turn always calls ``submit_plan``).
            execute_called = {
                e.get("name", "")
                for e in execute_result.tool_executions
                if e.get("success") is not False
            }
            plan_called = {
                e.get("name", "")
                for e in turn.plan_tool_executions
                if e.get("success") is not False
                and e.get("name", "") not in PLAN_META_TOOL_NAMES
            }

            # Delivery gate.  ``exe_catalogue`` / ``plan_phantom_tools``
            # were computed above when the Execute surface was built.
            #
            # The planner names tools it CANNOT SEE — its catalogue lists
            # MCP *servers* only, never tool names — so ``tools_needed``
            # routinely contains wrong guesses (e.g.
            # ``slack_conversations_postMessage`` for a server that
            # exposes ``slack_conversations_add_message``).  A wrong guess
            # must not let a non-delivery slip through: keying the gate
            # solely off catalogue-resolved names meant a plan whose only
            # delivery tool was a phantom read as "no action expected" and
            # the turn completed without ever posting (a "reply hi" that
            # produced text but never called Slack).
            real_tools_needed = {t for t in plan.tools_needed if t in exe_catalogue}

            # Tools that never count as a delivery (always-on + meta).
            # ``list_mcp_server_tools`` used to be unioned in by hand here
            # because it was missing from ``PLAN_META_TOOL_NAMES``; it now
            # lives in that set, so every consumer (this gate, Review's
            # ``## What Plan did`` log, the prior-work ledger) filters it
            # from one source of truth.
            non_delivery_tools = set(self._always_on) | set(PLAN_META_TOOL_NAMES)
            # The planner INTENDED a delivery if it named any non-meta
            # tool in ``tools_needed`` — phantom guesses INCLUDED.  Keying
            # intent off the raw ``tools_needed`` (not the
            # catalogue-filtered set) is what closes the
            # silent-non-delivery hole.
            expected_action = any(
                t not in non_delivery_tools for t in plan.tools_needed
            )

            # Names among everything called that are *positively known
            # reads* (annotation ``read_only`` is True), from the role's
            # MCP wrappers or the registry's builtin annotations.  The
            # phantom-guess delivery fallback skips these.
            all_called = execute_called | plan_called
            role_mcp_by_name = {t.name: t for t in role_mcp}
            registry_by_name = {t.name: t for t in self._tool_registry.list_tools()}
            # Annotations for each called tool, resolved from its instance
            # (role MCP wrapper or shared MCP wrapper / builtin in the
            # registry) with the registry's builtin side-table as a
            # fallback.  Resolving from the instance is what lets annotated
            # reads on *shared* MCP servers (e.g. a web-search server) be
            # excluded too — a per-role-only lookup would miss them.
            called_annotations = {
                name: resolve_annotations(
                    role_mcp_by_name.get(name) or registry_by_name.get(name),
                    self._tool_registry.annotations_for,
                )
                for name in all_called
            }
            # Names called that are *positively known reads* — skipped by
            # the phantom-guess delivery fallback.
            known_read_names = {
                name
                for name, ann in called_annotations.items()
                if _annotation_is_read(ann)
            }
            # Server-backed tool names (per-role MCP wrappers + shared MCP
            # tools registered globally).  A delivery to a shared surface
            # only comes from an MCP server, so the phantom-guess fallback
            # requires the called tool to be one of these — excluding
            # first-party builtins a phase may have called during recon
            # (lookup_colleague, reflect_and_persist, …) so they can't
            # mask a non-delivery.  (Residual: an *un-annotated* MCP read a
            # phase makes during recon still counts as a delivery here —
            # without a read hint we can't tell it apart from the real
            # write tool; Review remains the fine-grained judge on
            # non-``direct`` plans.)
            mcp_tool_names = {
                name
                for name, t in {**registry_by_name, **role_mcp_by_name}.items()
                if getattr(t, "server_name", "")
            }

            # Safety net for ``direct`` plans (the only ``skip_review``
            # path).  ``direct`` means the planner committed to Execute
            # doing the work in one shot — so Plan-phase delivery does
            # NOT count here.  If Execute alone didn't deliver, force
            # Review so the miss gets caught instead of a silent no-op.
            # (Code work runs via the run_sandbox tool, which the executor
            # follows with its own reply/PR tool — counted here like any
            # other Execute tool call.)
            execute_delivered = _phase_delivered(
                execute_called, real_tools_needed, mcp_tool_names, known_read_names
            )
            if skip_review and expected_action and not execute_delivered:
                logger.warning(
                    "review_forced_execute_skipped_delivery",
                    turn_id=turn.turn_id,
                    tools_needed=sorted(plan.tools_needed),
                    phantom=plan_phantom_tools,
                    tools_called=sorted(execute_called),
                )
                skip_review = False

            # Post-Review delivery view: Plan-phase calls count because
            # Review has already seen the full ``## What Plan did`` log
            # and judged with that context.  Used by the
            # ``done`` → ``self_iterate`` override below.
            called_tool_names = all_called
            delivered = _phase_delivered(
                called_tool_names,
                real_tools_needed,
                mcp_tool_names,
                known_read_names,
            )

            if skip_review:
                logger.info(
                    "review_skipped_per_plan",
                    turn_id=turn.turn_id,
                    decision=plan.decision,
                )
                turn.model_keys["review"] = ""
                final_artifact = execute_result.text or ""
                decision = "done"
                plan_notes = ""
                break

            rev_key, rev_provider = self._build_phase_provider(
                role, "review", agent_handle=turn.agent.handle
            )
            await _set_typing_phase(typing, "review")
            review = await self._child_phase(
                "agent.turn.review",
                run_review_phase,
                turn=turn,
                plan=plan,
                execute_result=execute_result,
                provider=rev_provider,
                provider_key=rev_key,
                registry=self._tool_registry,
                role_mcp_tools=role_mcp,
                event_queue=self._event_queue,
                agent_context=agent_context,
                budget_manager=self._budget_manager,
                observability=self._observability,
                token_usage_repo=self._token_usage_repo,
                validators=self._result_validators,
                prompt_skill_registry=self._prompt_skill_registry,
            )

            final_artifact = review.final_artifact or execute_result.text or ""
            decision = review.decision
            plan_notes = review.notes

            # Hard override: Review's LLM frequently judges from the
            # produced *text* and says "done" even when neither phase
            # called the action tool to deliver it (e.g. composed a
            # Slack reply but skipped ``slack_conversations_add_message``).
            # We already forced Review to run via ``skip_review = False``
            # above; here we also override its decision when the action
            # tools listed in ``tools_needed`` weren't actually called.
            # Plan-phase successful calls count here because Review
            # judged with the full ``## What Plan did`` context, so a
            # Plan-delivered action is genuine delivery — demanding
            # Execute repeat it would double-post.  Without this the
            # agent appears to have answered the user but the message
            # never reaches Slack / Jira / GitHub.
            if decision == "done" and expected_action and not delivered:
                if real_tools_needed:
                    missing = sorted(real_tools_needed - called_tool_names)
                    detail = (
                        "did not call the required delivery tool(s): "
                        + ", ".join(missing)
                        + ". Re-plan and ensure those tools are actually "
                        "invoked."
                    )
                else:
                    # Planner named only phantom guesses (MCP tool names
                    # it can't see and guessed wrong) and no real action
                    # tool was called.
                    detail = (
                        "named delivery tool(s) that don't exist in the "
                        "catalogue ("
                        + ", ".join(plan_phantom_tools)
                        + ") and no real action tool was called. Discover "
                        "the actual tool with `list_mcp_server_tools` + "
                        "`activate_tool`, then call it."
                    )
                logger.warning(
                    "review_done_overridden_undelivered",
                    turn_id=turn.turn_id,
                    tools_needed=plan.tools_needed,
                    tools_called=sorted(called_tool_names),
                    phantom=plan_phantom_tools,
                )
                decision = "self_iterate"
                plan_notes = (
                    (plan_notes + "\n\n" if plan_notes else "")
                    + "Execute produced an answer as text but "
                    + detail
                )

            if decision == "done":
                break
            # self_iterate is the only remaining decision: stall detection,
            # then loop back to Plan with the review notes.
            stall.observe(final_artifact)
            if stall.should_abort():
                logger.info("turn_stall_aborted", turn_id=turn.turn_id)
                await self._publish_guard_breach(
                    turn=turn,
                    kind="stall",
                    detail=(
                        "two self_iterate rounds produced the same "
                        "artifact hash; aborting turn"
                    ),
                )
                decision = "failed"
                break
            # Record this closed round so the next Plan / Execute pass —
            # each of which rebuilds its LLM conversation from scratch —
            # can see what already ran and plan only the gap.  Two
            # layers, deliberately:
            #
            # - the tool-call lists are ENGINE-recorded, so they cannot
            #   be forgotten.  That matters most on the ``done`` ->
            #   ``self_iterate`` override just above: Review decided
            #   ``done`` there and wrote no ``completed_work`` at all,
            #   yet it is exactly the path where a partial delivery may
            #   already have landed;
            # - ``completed_work`` is the reviewer's prose gloss, which
            #   the mechanical log cannot express ("the post landed and
            #   reads fine, follow up in-thread rather than re-posting").
            #
            # This replaces appending the notes to ``task_description``:
            # that mutation also leaked "Review notes: …" into the
            # knowledge-search queries, the sandbox brief, and the
            # episode publisher, all of which want the user's actual ask.
            # ``known_read_names`` rides along so the ledger can mark reads.
            # Tool RESULTS are deliberately not carried across iterations, so
            # the next round must be free to re-run a fetch — telling it "do
            # not repeat" a ``jira_get_issue`` would push it to invent the
            # data instead.  The set is the same annotation-derived one the
            # delivery gate above already resolved.
            turn.iteration_history.append(
                IterationRecord(
                    iteration=turn.iteration,
                    plan_summary=plan.summary(),
                    plan_tool_calls=tuple(turn.plan_tool_executions),
                    execute_tool_calls=tuple(execute_result.tool_executions),
                    read_only_names=tuple(sorted(known_read_names)),
                    execute_text=execute_result.text or "",
                    review_notes=plan_notes,
                    completed_work=review.completed_work,
                )
            )
        else:
            # Max iterations exhausted; terminate turn as failed.
            if decision != "done":
                logger.info("turn_max_iterations_exhausted", turn_id=turn.turn_id)
                await self._publish_guard_breach(
                    turn=turn,
                    kind="max_iter",
                    detail=(
                        f"plan/execute/review loop exhausted at "
                        f"{self._max_iterations} iterations without `done`"
                    ),
                )
                decision = "failed"

        if decision == "failed":
            return final_artifact or "(no output)", decision
        return final_artifact or "", decision

    def _build_phase_provider(
        self,
        role: Any,
        phase: str,
        *,
        agent_handle: str,
    ) -> tuple[str, Any]:
        """Resolve the phase's provider-fallback chain and wrap it as a single
        ``LLMProvider``-shaped wrapper.

        Returns ``(head_key, provider)``. ``head_key`` is the chain
        head -- a stable tag for span attributes and event sources.
        ``provider`` is the ``FallbackLLMProvider`` that walks the
        chain on retryable errors and propagates fatal ones. When the
        chain has exactly one entry the wrapper is still used (it's
        a thin no-op pass-through) so behaviour stays uniform.
        """
        chain = resolve_phase_chain(role, phase, self._llm_providers)
        head_key = chain[0][0]
        event_queue = self._event_queue

        async def _on_fallback(from_key: str, to_label: str, exc: Exception) -> None:
            # The wrapper awaits this hook before proceeding, so the
            # event is observed before the chain advances. Best-
            # effort: a publish failure logs and never aborts the
            # phase.
            from crewlet.events.types import ProviderFallback
            from crewlet.providers.errors import (
                AllCredentialsExhausted,
                classify,
            )

            if isinstance(exc, AllCredentialsExhausted):
                kind = "pool_exhausted"
            else:
                kind = classify(exc).value
            try:
                await event_queue.publish(
                    "crewlet.events.provider_fallback",
                    ProviderFallback(
                        source=role.name,
                        agent_handle=agent_handle,
                        phase=phase,
                        from_provider_key=from_key,
                        to_provider_key=to_label,
                        error_kind=kind,
                    ),
                )
            except Exception:
                logger.exception("provider_fallback_event_publish_failed")

        wrapper = FallbackLLMProvider(chain, on_fallback=_on_fallback)
        return head_key, wrapper

    async def _child_phase(
        self,
        span_name: str,
        fn: Any,
        **kwargs: Any,
    ) -> Any:
        """Dispatch one phase runner inside its span and its failure guard.

        Every phase the operator sees — onboarding, plan, execute, review —
        goes through here, which makes it the one place a *failed* phase can
        be recorded.  A runner that raises never reaches its own
        ``publish_phase_completed``, so without the guard the turn's last
        durable trace is the ``agent_phase_started`` that opened it and the
        dashboard shows an in-flight call that never answers.  The guard
        publishes that missing event with the failure attached and re-raises
        the original exception, so the turn-level handling below is unchanged.
        """
        turn: TurnContext | None = kwargs.get("turn")
        phase = span_name.rsplit(".", 1)[-1]
        if turn is None:
            with tracer.start_as_current_span(span_name):
                return await fn(**kwargs)
        async with phase_failure_guard(
            event_queue=self._event_queue,
            agent=turn.agent,
            turn_id=turn.turn_id,
            iteration=turn.iteration,
            phase=phase,
            provider_key=kwargs.get("provider_key", "") or "",
            trigger=describe_trigger(turn.trigger_event),
        ):
            with tracer.start_as_current_span(span_name):
                return await fn(**kwargs)

    async def _run_onboarding(
        self,
        turn: TurnContext,
        agent_context: Any,
        role: Any,
        role_mcp: list[Any],
        typing: Any = None,
    ) -> bool:
        """Run the dedicated first-turn onboarding pass; return whether it ran.

        Resolves the Plan-phase provider chain (onboarding is plan-adjacent
        recon) and dispatches :func:`run_onboarding_phase` on its own round
        budget. The function self-skips (returns ``False``) when the agent is
        already onboarded or the onboarding machinery isn't wired — which is
        why the Slack status swap rides its ``on_start`` hook rather than
        happening here: every turn after the first would otherwise flash
        "is getting up to speed…" on its way to a skip.
        """
        from crewlet.agent.onboarding_phase import run_onboarding_phase

        key, provider = self._build_phase_provider(
            role, "plan", agent_handle=turn.agent.handle
        )
        # The round-cap extension judge governs onboarding too (its own
        # ceiling): a near-done pass that exhausts the base cap gets extended
        # rather than cut off, just like Plan/Execute.
        judge_key, judge_provider = self._build_phase_provider(
            role, "judge", agent_handle=turn.agent.handle
        )
        return await self._child_phase(
            "agent.turn.onboarding",
            run_onboarding_phase,
            turn=turn,
            provider=provider,
            provider_key=key,
            registry=self._tool_registry,
            role_mcp_tools=role_mcp,
            event_queue=self._event_queue,
            agent_context=agent_context,
            max_rounds=self._onboarding_max_tool_rounds,
            judge_provider=judge_provider,
            judge_provider_key=judge_key,
            extension_enabled=self._extension_enabled,
            extension_ceiling=self._onboarding_max_tool_rounds_ceiling,
            extension_round_step=self._extension_round_step,
            budget_manager=self._budget_manager,
            observability=self._observability,
            token_usage_repo=self._token_usage_repo,
            validators=self._result_validators,
            on_start=lambda: _set_typing_phase(typing, "onboarding"),
        )

    def _model_split_enabled(self, role: Any) -> bool:
        """Detect whether Plan / Execute / Review will actually run on
        different LLMs for this role.

        Resolves each phase's provider through the same
        :func:`resolve_phase_provider` the runtime uses, then checks
        whether the resolved keys differ.  This correctly flags the
        common case of "override just ``llm_execute`` to a cheaper
        model, leave ``llm_plan`` / ``llm_review`` unset" -- the
        previous implementation compared only the per-phase role
        fields, missed the fallback to ``role.llm`` / ``"default"``,
        and would return False for that setup even though the phase
        models actually differ (leaving the Plan prompt without the
        "executor runs on a cheaper / different model" hint).

        No providers configured (shouldn't happen in a live turn) =>
        False, preserving the hint-off default.
        """
        if not self._llm_providers:
            return False
        keys: set[str] = set()
        for phase in ("plan", "execute", "review"):
            try:
                key, _ = resolve_phase_provider(role, phase, self._llm_providers)
            except RuntimeError:
                return False
            keys.add(key)
        return len(keys) > 1

    def _build_agent_context(self, turn: TurnContext) -> AgentContext:
        from crewlet.org.hierarchy import get_unit_chain_for_role

        org = turn.org
        org_id = ""
        unit_ids: list[str] = []
        if org is not None:
            org_id = org.name
            role_obj = org.get_role(turn.agent.role_name)
            if role_obj is not None:
                chain = get_unit_chain_for_role(role_obj, org)
                unit_ids = [u.name for u in reversed(chain)]
        ctx = AgentContext(
            agent_id=turn.agent.id_str,
            agent_handle=turn.agent.handle,
            role=turn.agent.role_name,
            current_task_id=turn.task_id,
            org_id=org_id,
            unit_ids=unit_ids,
            org=org,
            event_queue=self._event_queue,
            budget_manager=self._budget_manager,
            storage=self._storage,
            knowledge_searcher=self._knowledge_searcher,
            notification_service=self._notification_service,
            a2a_service=self._a2a_service,
            handle_registry=self._handle_registry,
            counterparty_store=self._counterparty_store,
            synthesized_skill_store=self._synthesized_skill_store,
            episode_store=self._episode_store,
            agent_diary=self._agent_diary,
            onboarding_marker_store=self._onboarding_marker_store,
            last_notification_metadata=turn.notification_metadata or {},
            prompt_skill_registry=self._prompt_skill_registry,
            seat_fence=self._fence_for(turn.agent.handle),
        )
        # Attach the tool registry so colleague tools can forward to MCP.
        ctx.__dict__["tool_registry"] = self._tool_registry
        # Attach the TurnContext so colleague tools can read the current
        # delegation_depth / delegation_chain when emitting outbound
        # events.
        ctx.__dict__["turn_context"] = turn
        # Attach per-turn sub-agent spawn config so the spawn_subagent
        # tool can reach everything it needs without the tool itself
        # holding engine references.  ``parent_tool_names`` is filled
        # in just before the Execute phase (see ``_drive_phases``) with
        # the actual Execute ToolSurface's names -- doing it here
        # with the full registry would let a parent grant its sub-agent
        # tools the parent itself didn't have access to.
        role_mcp_tools = self._role_mcp_for(turn.agent.role_name, turn.agent.id_str)
        ctx.__dict__["spawn_subagent_config"] = {
            "llm_providers": self._llm_providers,
            "tool_registry": self._tool_registry,
            "role_mcp_tools": role_mcp_tools,
            "parent_tool_names": [],
            "max_turns_cap": self._subagent_max_turns,
            "timeout_seconds": self._subagent_timeout_seconds,
            "budget_fraction": self._subagent_budget_fraction,
            # Batched spawn_subagent knobs -- without these in
            # the dict the tool falls back to its hardcoded defaults
            # and the YAML config is silently ignored.
            "subagent_max_parallel": self._subagent_max_parallel,
            "subagent_batch_timeout_seconds": self._subagent_batch_timeout_seconds,
            "subagent_min_per_child_tokens": self._subagent_min_per_child_tokens,
            "budget_manager": self._budget_manager,
            "observability": self._observability,
            "token_usage_repo": self._token_usage_repo,
            "validators": self._result_validators,
            "prompt_skill_registry": self._prompt_skill_registry,
        }
        # Per-turn sandbox config for the run_sandbox tool:
        # everything launch_sandbox_run needs without the tool
        # holding engine references. Present even when no manager is wired;
        # the tool checks for it and the tool itself is gated off for
        # non-sandbox roles via its check_fn (sandbox_enabled).
        ctx.__dict__["sandbox_config"] = {
            "manager": self._sandbox_manager,
            "pending_store": self._sandbox_pending_store,
            "llm_providers": self._llm_providers,
            "llm_provider_configs": self._llm_provider_configs,
            "budget_manager": self._budget_manager,
            "mcp_server_configs": self._sandbox_mcp_servers,
            "otel_receiver": self._sandbox_otel_receiver,
            "sandbox_budget_fraction": self._sandbox_budget_fraction,
            "sandbox_min_budget_tokens": self._sandbox_min_budget_tokens,
        }
        return ctx

    async def _publish(self, event: Event, *, turn: TurnContext | None = None) -> None:
        """Publish ``event`` on the engine's event queue.

        When ``turn`` is given, enrich the event with delegation
        bookkeeping from the TurnContext before publishing, so that
        every lifecycle / guard / completion event emitted during a
        delegated turn carries the same ``delegation_depth`` /
        ``parent_turn_id`` / ``delegation_chain`` as the trigger
        event.  Downstream dashboards and traces need this to
        correlate the full chain of responsibility; without it,
        ``Event`` defaults (depth 0, empty chain) make delegated
        turns indistinguishable from top-level ones.

        We enrich only when the field is still at its default so
        event constructors that explicitly set a non-default value
        (none today, but defensively) are not overwritten.
        """
        if turn is not None:
            if not event.delegation_depth:
                event.delegation_depth = turn.delegation_depth
            if not event.parent_turn_id:
                event.parent_turn_id = turn.parent_turn_id
            if not event.delegation_chain:
                event.delegation_chain = list(turn.delegation_chain)
        await self._event_queue.publish(f"crewlet.events.{event.type}", event)

    async def _publish_guard_breach(
        self,
        *,
        turn: TurnContext,
        kind: str,
        detail: str,
    ) -> None:
        """Emit a TurnGuardBreach event for dashboard visibility.

        Guards that raise or short-circuit a turn are already logged via
        structlog; this wraps each one in a structured event so the
        TimescaleDB events table records them alongside task events.
        """
        # The turn-completed record reads this back: a stall abort or an
        # exhausted iteration cap ends the turn by RETURNING "failed",
        # not by raising, so without it the turn row said "failed" with
        # no cause while the reason sat on a separate event the LLM
        # history view does not read.
        turn.guard_breach = {"kind": kind, "detail": detail}
        try:
            await self._publish(
                TurnGuardBreach(
                    source=turn.agent.role_name,
                    agent_id=turn.agent.id_str,
                    role=turn.agent.role_name,
                    kind=kind,
                    detail=detail,
                    turn_id=turn.turn_id,
                ),
                turn=turn,
            )
        except Exception:
            logger.exception("guard_breach_publish_failed", kind=kind)

    async def _publish_agent_turn_completed(
        self,
        turn: TurnContext,
        final_text: str,
        decision: str,
        *,
        turn_succeeded: bool = True,
        failure: BaseException | None = None,
    ) -> None:
        # A turn fails two ways: an exception (the guard re-raises it), or
        # a guard that ends the loop by returning ``decision="failed"``.
        # Both have to reach the record, or the dashboard shows a turn
        # that stopped for no stated reason.
        failed = not turn_succeeded or decision == "failed"
        error, error_kind = ("", "")
        if failure is not None:
            error, error_kind = describe_failure(failure)
        elif failed:
            breach = getattr(turn, "guard_breach", None) or {}
            error = str(breach.get("detail", ""))
            error_kind = str(breach.get("kind", ""))
        try:
            event = AgentTurnCompleted(
                source=turn.agent.role_name,
                agent_id=turn.agent.id_str,
                role=turn.agent.role_name,
                trigger=describe_trigger(turn.trigger_event),
                model=turn.model_keys.get("plan", "")
                or turn.model_keys.get("execute", "")
                or turn.model_keys.get("review", ""),
                prompt=(turn.task_description or "")[:200],
                prompt_messages=[],
                response=final_text,
                input_tokens=turn.input_tokens,
                output_tokens=turn.output_tokens,
                total_tokens=turn.input_tokens + turn.output_tokens,
                tool_executions=[],
                a2a_context=turn.a2a_context,
                turn_id=turn.turn_id,
                plan_model=turn.model_keys.get("plan", ""),
                execute_model=turn.model_keys.get("execute", ""),
                review_model=turn.model_keys.get("review", ""),
                subagent_count=turn.subagent_count,
                subagent_tokens=turn.subagent_tokens,
                iterations=turn.iteration,
                decision=decision,
                failed=failed,
                error=error,
                error_kind=error_kind,
            )
            # ``_publish(turn=turn)`` populates ``delegation_depth``,
            # ``parent_turn_id``, and ``delegation_chain`` on the event
            # so dashboards can correlate the full delegation tree
            # (see ``Event`` base-class docstring).
            await self._publish(event, turn=turn)
        except Exception:
            logger.exception("agent_turn_completed_publish_failed")

        # --- Agent-learning subsystem: emit TurnCompleted + write Episode ---
        # Separated from the AgentTurnCompleted publish above so the
        # dashboard's single-phase summary remains unchanged.  Skipped on
        # turn crashes — ``decision`` would still be the default ``"done"``
        # and ``last_plan`` / ``last_execute_result`` may be missing or
        # incoherent; persisting that as an episode would poison
        # similarity search and PersistDecider.
        if not turn_succeeded:
            logger.debug("learning_emit_skipped_turn_failed", turn_id=turn.turn_id)
            return
        # A turn that only SUSPENDED for a detached sandbox run hasn't finished —
        # the coding job is still running and the resumed turn (same turn_id)
        # will reach the real terminal state. Emitting TurnCompleted now would
        # (a) reflect on an incomplete turn (no sandbox result, no delivery) and
        # (b) mark the turn_id seen in the ReflectEngine's dedup set, so the
        # genuine completion is later dropped as a duplicate. Skip the learning
        # emit on suspend; the resumed completion emits it once, for real.
        last_exec = turn.last_execute_result
        if last_exec is not None and getattr(last_exec, "status", "") == "detached":
            logger.debug("learning_emit_skipped_suspended", turn_id=turn.turn_id)
            return
        await self._publish_turn_completed(turn, decision)
        await self._write_episode(turn, decision)

    async def _publish_turn_completed(self, turn: TurnContext, decision: str) -> None:
        """Emit the learning-shaped ``TurnCompleted`` event."""
        try:
            d = _derive_turn_metrics(turn)
            interactions = turn.trigger_interactions()
            # Surface the planner's top-level decision so the
            # ReflectEngine can short-circuit learning on "skip" turns
            # -- the agent explicitly opted out and persisting facts
            # read off a trigger meant for someone else would teach it
            # phantom directives.
            plan_decision = ""
            if turn.last_plan is not None:
                plan_decision = str(getattr(turn.last_plan, "decision", "") or "")
            event = TurnCompleted(
                source=turn.agent.role_name,
                agent_id=turn.agent.id_str,
                agent_handle=turn.agent.handle or "",
                role=turn.agent.role_name,
                turn_id=turn.turn_id,
                task_id=turn.task_id or "",
                started_at=d.started_at,
                ended_at=d.ended_at,
                duration_ms=d.duration_ms,
                task_summary=(turn.task_description or "")[:2000],
                plan_summary=d.plan_summary,
                tool_sequence=d.tool_sequence,
                plan_tool_sequence=list(turn.plan_tool_sequence),
                skills_used=d.skills_used,
                review_outcome=decision,
                iterations=turn.iteration,
                plan_decision=plan_decision,
                interactions=[i for i in interactions if i.has_sender],
            )
            await self._publish(event, turn=turn)
        except Exception:
            logger.exception("turn_completed_publish_failed")

    async def _write_episode(self, turn: TurnContext, decision: str) -> None:
        """Persist one episode row to the learning store.

        No-op when the engine was wired without an ``episode_store``
        (in-memory mode or ``learning.enabled=false``) — the exception
        guard here is belt-and-braces so a DB outage never breaks turn
        completion.
        """
        if self._episode_store is None:
            return
        with tracer.start_as_current_span(
            "learning.episode_write",
            attributes={
                "learning.worker": "episode_write",
                "learning.turn_id": turn.turn_id,
                "learning.agent_handle": turn.agent.handle or "",
                "learning.role": turn.agent.role_name,
            },
        ) as span:
            try:
                d = _derive_turn_metrics(turn)
                if decision in _VALID_EPISODE_OUTCOMES:
                    review_outcome = decision
                else:
                    span.set_attribute("learning.outcome_coerced", True)
                    logger.warning(
                        "episode_unknown_review_outcome_coerced",
                        decision=decision,
                        review_outcome="done",
                        turn_id=turn.turn_id,
                    )
                    review_outcome = "done"
                try:
                    turn_uuid = UUID(turn.turn_id)
                except (TypeError, ValueError):
                    span.set_attribute("learning.outcome", "skipped")
                    span.set_attribute("learning.skip_reason", "bad_turn_id")
                    logger.warning("episode_skipped_bad_turn_id", turn_id=turn.turn_id)
                    return
                episode = Episode(
                    agent_handle=turn.agent.handle or "",
                    agent_role=turn.agent.role_name,
                    task_id=turn.task_id or "",
                    turn_id=turn_uuid,
                    started_at=d.started_at,
                    ended_at=d.ended_at,
                    plan_summary=d.plan_summary,
                    task_summary=(turn.task_description or "")[:2000],
                    tool_sequence=d.tool_sequence,
                    skills_used=d.skills_used,
                    review_outcome=review_outcome,  # type: ignore[arg-type]
                    duration_ms=d.duration_ms,
                    # Ambient rather than threaded through the turn: the
                    # engine binds it around the dispatch, and every
                    # frame between here and there has no other reason
                    # to carry it. Empty for a turn with no ledgerable
                    # trigger, which is the honest answer — there is no
                    # cross-node duplicate to collapse.
                    work_key=current_work_key(),
                )
                await self._episode_store.write(episode)
                span.set_attribute("learning.outcome", "done")
                span.set_attribute("learning.review_outcome", review_outcome)
                span.set_attribute("learning.duration_ms", d.duration_ms)
                span.set_attribute("learning.tool_count", len(d.tool_sequence))
                # Lifecycle event for the dashboard's trace-grouped view.
                # Trace context auto-captured from the surrounding
                # ``learning.episode_write`` span so it groups with the
                # turn that produced it.
                try:
                    await self._publish(
                        EpisodeWritten(
                            source=turn.agent.role_name,
                            agent_id=turn.agent.id_str,
                            agent_handle=turn.agent.handle or "",
                            role=turn.agent.role_name,
                            turn_id=turn.turn_id,
                            review_outcome=review_outcome,
                            duration_ms=d.duration_ms,
                            tool_count=len(d.tool_sequence),
                        ),
                        turn=turn,
                    )
                except Exception:
                    logger.exception(
                        "episode_written_publish_failed", turn_id=turn.turn_id
                    )
            except Exception as exc:
                span.set_attribute("learning.outcome", "failed")
                set_span_error(exc)
                logger.exception("episode_write_failed", turn_id=turn.turn_id)


_VALID_EPISODE_OUTCOMES = {"done", "self_iterate", "failed"}


@dataclass(frozen=True)
class _TurnDerivation:
    """Derived turn metrics shared between ``TurnCompleted`` event
    emission and ``Episode`` persistence.

    Both consumers compute the same six fields from ``TurnContext``;
    centralising the derivation here keeps the event payload and the
    persisted row from drifting on field semantics or rounding.
    """

    started_at: datetime
    ended_at: datetime
    duration_ms: int
    plan_summary: str
    tool_sequence: list[str]
    skills_used: list[str]


def _derive_turn_metrics(turn: TurnContext) -> _TurnDerivation:
    ended_at = datetime.now(UTC)
    started_at = turn.started_at if turn.started_at is not None else ended_at
    duration_ms = max(0, int((ended_at - started_at).total_seconds() * 1000))
    plan_summary = _safe_plan_summary(turn.last_plan)
    tool_sequence, skills_used = _extract_tool_sequence_and_skills(
        turn.last_execute_result
    )
    return _TurnDerivation(
        started_at=started_at,
        ended_at=ended_at,
        duration_ms=duration_ms,
        plan_summary=plan_summary,
        tool_sequence=tool_sequence,
        skills_used=skills_used,
    )


def _safe_plan_summary(plan: Any) -> str:
    """Render the plan summary without failing on partial-turn states."""
    if plan is None:
        return ""
    try:
        summary = plan.summary() if hasattr(plan, "summary") else ""
    except Exception:
        summary = ""
    return (summary or "")[:2000]


def _extract_tool_sequence_and_skills(
    execute_result: Any,
) -> tuple[list[str], list[str]]:
    """Return ``(tool_sequence, skills_used)`` from an ExecuteResult.

    ``tool_sequence`` preserves call order.  ``skills_used`` is
    derived from ``use_skill`` invocations by parsing the tool's
    ``skill_name`` argument out of the tool-execution trace.
    """
    if execute_result is None:
        return [], []
    executions = getattr(execute_result, "tool_executions", None) or []
    tool_sequence: list[str] = []
    skills_used: list[str] = []
    seen_skills: set[str] = set()
    for exe in executions:
        name = exe.get("name", "")
        if not name:
            continue
        tool_sequence.append(name)
        if name == "use_skill":
            args_raw = exe.get("arguments", "")
            if isinstance(args_raw, str):
                try:
                    args = json.loads(args_raw) if args_raw else {}
                except Exception:
                    args = {}
            elif isinstance(args_raw, dict):
                args = args_raw
            else:
                args = {}
            skill_name = str(args.get("skill_name", "")).strip()
            if skill_name and skill_name not in seen_skills:
                skills_used.append(skill_name)
                seen_skills.add(skill_name)
    return tool_sequence, skills_used


__all__ = ["TurnEngine"]
