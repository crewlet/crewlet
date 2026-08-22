"""Ephemeral sub-agent spawning.

The parent's Execute phase can call :func:`spawn_subagent` to run
a short-lived bespoke worker with a parent-chosen tool allowlist.

Invariants enforced here (in code, not prompts):

1. Tool allowlist must be a subset of the parent's tools, minus
   ``spawn_subagent`` itself and every colleague-surface tool.
   :func:`~crewlet.tools.surface.ToolSurface.for_subagent` does the
   filter and returns the rejected names for tracing.
2. The sub-agent gets the requested tools' schemas PLUS the discovery
   meta-tools (``list_mcp_server_tools`` / ``activate_tool``) over a
   safety-filtered catalogue (:func:`subagent_safe_tools`), so it can
   find and activate read-only / non-control / non-shared-write tools
   the parent didn't name. Discovery cannot breach invariant #1 — the
   catalogue is pre-filtered by the same rules.
3. System prompt is the parent-provided task prompt + the mandated
   :data:`~crewlet.agent.prompts.SUBAGENT_PREAMBLE`.
4. Budget is runtime-controlled. ``max_turns`` is capped at the
   runtime's configured maximum; token budget is a slice of the
   parent's remaining budget.
5. Fresh context: empty message history, seeded with ``task_prompt``
   as the first user message.
6. Timeout: wrapped in :func:`asyncio.wait_for`.
7. Tracing: each spawn emits a distinct ``agent.subagent`` span; the
   parent's Execute span is the parent span.
"""

from __future__ import annotations

import asyncio
import hashlib
from dataclasses import dataclass
from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.llm_loop import (
    phase_failure_guard,
    publish_phase_completed,
    run_tool_loop,
)
from crewlet.agent.prompts import build_subagent_prompt
from crewlet.agent.skills.guard import skill_guard_for_turn
from crewlet.agent.skills.models import Phase as SkillPhase
from crewlet.agent.tool_discovery import (
    build_activate_tool,
    build_list_mcp_server_tools,
)
from crewlet.agent.turn_context import TurnContext
from crewlet.events.types import describe_trigger
from crewlet.providers.llm.protocol import LLMProvider, Message
from crewlet.queue.protocol import EventQueue
from crewlet.telemetry import tracer
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry
from crewlet.tools.surface import ToolSurface, subagent_safe_tools

logger = get_logger("agent.subagent")


class SubagentBudgetExceeded(RuntimeError):
    """Raised when a sub-agent's fractional budget cap is breached."""


class _FractionalBudgetManager:
    """Wraps a real ``BudgetManager`` for the duration of one sub-agent
    call, capping total consumption at ``fraction *
    parent_agent_remaining`` (or a flat cap when there is no agent budget).

    Exposes the same duck-typed surface ``llm_loop.consume_budget`` uses:
    ``spend(agent_id, tokens)`` (awaitable, returning a ``SpendOutcome``),
    ``consume(...)``, ``org_budget``, ``get_agent_budget(agent_id)``.
    Tokens still flow through the real manager for org/agent accounting
    against the shared counter; the wrapper just adds a per-call ceiling.

    On cap breach the wrapper raises :class:`SubagentBudgetExceeded`
    rather than returning ``False`` -- that way ``llm_loop.consume_budget``
    doesn't attempt to publish a misleading ``budget_exhausted`` event
    pointing at the parent's agent budget.
    """

    def __init__(
        self,
        inner: Any,
        *,
        max_subagent_tokens: int,
    ) -> None:
        self._inner = inner
        self._max_subagent_tokens = max(0, max_subagent_tokens)
        self._subagent_used = 0
        # One wrapper is shared by every child of a batched spawn,
        # and those children run concurrently under ``asyncio.gather``.
        # ``consume`` straddles an ``await`` (the inner charge), so the
        # cap-check + reservation must be atomic or two children both
        # pass the check against the same ``_subagent_used`` snapshot
        # and overshoot the cap.
        self._lock = asyncio.Lock()

    @property
    def org_budget(self):  # type: ignore[override]
        return self._inner.org_budget

    def get_agent_budget(self, agent_id: str):  # type: ignore[override]
        return self._inner.get_agent_budget(agent_id)

    async def spend(self, agent_id: str, tokens: int) -> Any:
        # Reserve under the lock *before* the inner await so a
        # concurrent child sees the reservation and can't also pass the
        # cap check.  The reservation is given back if the inner
        # manager refuses the charge.
        async with self._lock:
            if (
                self._max_subagent_tokens > 0
                and self._subagent_used + tokens > self._max_subagent_tokens
            ):
                logger.warning(
                    "subagent_budget_exhausted",
                    agent_id=agent_id,
                    subagent_used=self._subagent_used,
                    cap=self._max_subagent_tokens,
                    requested=tokens,
                )
                raise SubagentBudgetExceeded(
                    f"sub-agent budget exhausted: "
                    f"used={self._subagent_used} + requested={tokens} "
                    f"> cap={self._max_subagent_tokens}"
                )
            self._subagent_used += tokens
        outcome = await self._inner.spend(agent_id, tokens)
        if not outcome.ok:
            async with self._lock:
                self._subagent_used -= tokens
        return outcome

    async def consume(self, agent_id: str, tokens: int) -> bool:
        """Boolean form, matching :meth:`BudgetManager.consume`."""
        return (await self.spend(agent_id, tokens)).ok


def _subagent_budget_cap(
    *,
    budget_manager: Any,
    parent_agent_id: str,
    fraction: float,
) -> int:
    """Compute the absolute token cap for a sub-agent spawn.

    Falls back to ``0`` (unlimited) when no agent-level budget is set.
    """
    if budget_manager is None or fraction <= 0 or fraction > 1:
        return 0
    agent_budget = budget_manager.get_agent_budget(parent_agent_id)
    if agent_budget is None or agent_budget.max_tokens <= 0:
        # No per-agent cap → don't impose one on sub-agents either.
        return 0
    remaining = max(0, agent_budget.max_tokens - agent_budget.used_tokens)
    return max(1, int(remaining * fraction))


@dataclass
class SubagentResult:
    """Outcome of one :func:`spawn_subagent` call."""

    text: str
    turns_used: int
    tokens_used: int
    rejected_tools: list[str]
    timed_out: bool = False
    model: str = ""
    error: str = ""
    """Set when a batched spawn caught an exception for this
    child. Empty for successful calls. Capped at 500 chars by the
    batch runner so a verbose stack trace can't blow up the tool
    result payload."""


def _build_subagent_meta_tools(
    *,
    parent_turn: TurnContext,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    event_queue: EventQueue,
) -> tuple[list[Any], list[Any]]:
    """Build a sub-agent's discovery meta-tools + their surface holder.

    Returns ``(meta_tools, surface_holder)``. The caller passes
    ``meta_tools`` to :meth:`ToolSurface.for_subagent`, then writes the
    constructed surface into ``surface_holder[0]`` so ``activate_tool``'s
    closure can read the live surface (the same chicken-and-egg the
    Plan / Execute phases solve). ``list_mcp_server_tools`` is scoped to
    the safety-filtered MCP subset so the sub-agent can only discover
    tools it is actually allowed to activate.
    """
    surface_holder: list[Any] = [None]
    safe_catalogue = subagent_safe_tools(
        registry, role_mcp_tools, availability_filter=parent_turn.availability_set
    )
    safe_names = {t.name for t in safe_catalogue}
    safe_mcp = [t for t in role_mcp_tools if t.name in safe_names]
    meta_tools = [
        build_activate_tool(
            safe_catalogue,
            surface_holder,
            phase="subagent",
            event_queue=event_queue,
            agent_id=parent_turn.agent.id_str,
            agent_role=parent_turn.agent.role_name,
            turn_id=parent_turn.turn_id,
            iteration=parent_turn.iteration,
        ),
        build_list_mcp_server_tools(
            safe_mcp,
            availability_filter=parent_turn.availability_set,
        ),
    ]
    return meta_tools, surface_holder


async def _publish_subagent_failure(
    *,
    event_queue: EventQueue,
    parent_turn: Any,
    provider_key: str,
    progress: Any,
    surface: Any,
    trigger: dict[str, Any],
    error: str,
    error_kind: str,
) -> None:
    """Emit the ``phase="subagent"`` event for a sub-agent that was cut off.

    The timeout and budget-exhausted paths *return* a
    :class:`SubagentResult` rather than raising, so
    :func:`~crewlet.agent.llm_loop.phase_failure_guard` never sees them.
    Without this the spawn published nothing: the parent's Execute event
    showed a ``spawn_subagent`` tool call whose sub-agent left no phase
    record, no partial transcript, and no reason it stopped.
    """
    await publish_phase_completed(
        event_queue=event_queue,
        agent=parent_turn.agent,
        turn_id=parent_turn.turn_id,
        conversation_key=parent_turn.stored_conversation_key,
        iteration=parent_turn.iteration,
        phase="subagent",
        provider_key=provider_key,
        loop=progress.to_result(),
        tools_available=list(surface.names),
        trigger=trigger,
        tag_span=False,
        failed=True,
        error=error,
        error_kind=error_kind,
    )


async def spawn_subagent(
    *,
    parent_turn: TurnContext,
    task_prompt: str,
    system_prompt: str,
    tool_names: list[str],
    parent_tool_names: list[str],
    provider: LLMProvider,
    provider_key: str,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    agent_context: AgentContext,
    event_queue: EventQueue,
    max_turns_cap: int = 20,
    timeout_seconds: float = 120.0,
    max_turns_request: int | None = None,
    budget_fraction: float = 0.2,
    budget_manager: Any = None,
    observability: Any = None,
    token_usage_repo: Any = None,
    validators: list[Any] | None = None,
    prompt_skill_registry: Any = None,
) -> SubagentResult:
    """Run a short-lived sub-agent and return its text output.

    ``tool_names`` -- the names the parent is asking to grant. Will be
    intersected with ``parent_tool_names``, filtered against the
    first-party ``SUBAGENT_CONTROL_DENYLIST``, and stripped of any tool
    whose MCP annotations mark it a write to an external shared surface
    (``writes_to_shared_surface``).

    ``max_turns_request`` -- optional parent-requested turn cap.
    Clamped to ``max_turns_cap``; the parent can ask for less, not
    more.
    """
    meta_tools, surface_holder = _build_subagent_meta_tools(
        parent_turn=parent_turn,
        registry=registry,
        role_mcp_tools=role_mcp_tools,
        event_queue=event_queue,
    )
    surface, rejected = ToolSurface.for_subagent(
        registry,
        role_mcp_tools,
        parent_tool_names=parent_tool_names,
        requested_tool_names=tool_names,
        availability_filter=parent_turn.availability_set,
        meta_tools=meta_tools,
    )
    surface_holder[0] = surface
    if rejected:
        logger.info("subagent_tools_rejected", rejected=rejected)
    # Required-skill guard: the sub-agent runs on a fresh message
    # history, so any required skill covering its granted tools must be
    # loaded inside THIS session (the surface always carries
    # ``load_tool_skill`` for exactly that).
    surface.skill_guard = skill_guard_for_turn(
        registry=prompt_skill_registry,
        phase=SkillPhase.SUBAGENT,
        surface=surface,
        turn=parent_turn,
        event_queue=event_queue,
    )

    # Clamp turns to the runtime cap; default to cap when unspecified.
    requested = max_turns_cap if max_turns_request is None else max_turns_request
    if requested < 1:
        requested = 1
    effective_max_turns = min(requested, max_turns_cap)

    # Cap the sub-agent's token usage at a fraction of the parent's
    # remaining agent budget (invariant 4 in the module docstring).  When
    # there is no agent-level budget, the wrapper is a pass-through.
    effective_budget_manager: Any = budget_manager
    if budget_manager is not None:
        cap = _subagent_budget_cap(
            budget_manager=budget_manager,
            parent_agent_id=parent_turn.agent.id_str,
            fraction=budget_fraction,
        )
        if cap > 0:
            effective_budget_manager = _FractionalBudgetManager(
                budget_manager, max_subagent_tokens=cap
            )

    # Assemble the sub-agent prompt. System section is preamble + parent
    # prompt; user message carries the task_prompt verbatim.
    final_system = build_subagent_prompt(
        parent_turn.agent.definition,
        parent_system_prompt=system_prompt,
        # Surface skills for both the initially-active tools AND anything
        # the sub-agent might discover+activate from its catalogue, so a
        # required skill is loadable before the first call (mirrors
        # Execute's catalogue-scoped skill injection).
        available_tools=sorted(set(surface.names) | set(surface.catalogue_names())),
        tool_catalogue=surface.catalogue_text(),
        skill_registry=prompt_skill_registry,
    )
    messages: list[Message] = [
        Message(role="system", content=final_system),
        Message(role="user", content=task_prompt),
    ]

    # Hash the final assembled system prompt (parent task prompt +
    # runtime preamble) for the span attribute.  Short SHA-256 prefix
    # is sufficient for tracing / identity purposes.
    system_prompt_hash = hashlib.sha256(
        final_system.encode("utf-8", errors="replace")
    ).hexdigest()[:16]

    # Mirror the inner loop's running round-count / token-count so the
    # timeout and budget-exhausted paths can report real partial usage
    # rather than hard-coded zeros.  ``run_tool_loop`` calls
    # ``on_progress`` at the end of every round, so by the time a
    # mid-round exception propagates out, ``observed`` carries the
    # state from the last fully-completed round.  For the budget
    # path we additionally trust the ``_FractionalBudgetManager``'s
    # ``_subagent_used`` counter, which is the most precise figure
    # for tokens the sub-agent actually consumed.
    observed = {"rounds_used": 0, "tokens_used": 0}
    trigger = describe_trigger(parent_turn.trigger_event)

    async def _record_progress(progress: Any) -> None:
        # ``AgentTurnProgress.round_num`` is zero-based; convert to the
        # 1-based "rounds completed" value ``SubagentResult.turns_used``
        # documents.
        observed["rounds_used"] = int(getattr(progress, "round_num", 0)) + 1
        observed["tokens_used"] = int(getattr(progress, "total_tokens", 0))

    # A sub-agent is its own phase on the dashboard timeline, so it records
    # its own failures rather than letting them surface only as the host
    # Execute phase dying. The guard shadows Execute's for this block.
    async with phase_failure_guard(
        event_queue=event_queue,
        agent=parent_turn.agent,
        turn_id=parent_turn.turn_id,
        iteration=parent_turn.iteration,
        phase="subagent",
        provider_key=provider_key,
        trigger=trigger,
        conversation_key=parent_turn.stored_conversation_key,
    ) as sub_progress:
        with tracer.start_as_current_span(
            "agent.subagent",
            attributes={
                "agent.id": parent_turn.agent.id_str,
                "subagent.tool_names": ",".join(surface.names),
                "subagent.max_turns": effective_max_turns,
                "subagent.system_prompt_hash": system_prompt_hash,
            },
        ) as span:
            try:
                loop = await asyncio.wait_for(
                    run_tool_loop(
                        provider=provider,
                        messages=messages,
                        surface=surface,
                        context=agent_context,
                        agent=parent_turn.agent,
                        max_rounds=effective_max_turns,
                        event_queue=event_queue,
                        budget_manager=effective_budget_manager,
                        observability=observability,
                        token_usage_repo=token_usage_repo,
                        validators=validators,
                        provider_key=provider_key,
                        on_progress=_record_progress,
                        a2a_context=None,  # sub-agents do not inherit a2a ctx
                        # Matches the ``phase="subagent"`` AgentPhaseCompleted
                        # this spawn publishes under the parent's turn.
                        turn_id=parent_turn.turn_id,
                        iteration=parent_turn.iteration,
                        trigger=trigger,
                    ),
                    timeout=timeout_seconds,
                )
            except TimeoutError:
                partial_turns = observed["rounds_used"]
                partial_tokens = observed["tokens_used"]
                logger.warning(
                    "subagent_timed_out",
                    timeout=timeout_seconds,
                    agent_id=parent_turn.agent.id_str,
                    turns_used=partial_turns,
                    tokens_used=partial_tokens,
                )
                span.set_attribute("subagent.turns_used", partial_turns)
                span.set_attribute("subagent.tokens_used", partial_tokens)
                span.set_attribute("subagent.timed_out", True)
                parent_turn.subagent_count += 1
                parent_turn.subagent_tokens += partial_tokens
                # This path returns a result instead of raising, so the guard
                # never fires — publish the failed phase here or the sub-agent
                # leaves no trace at all on the dashboard timeline.
                await _publish_subagent_failure(
                    event_queue=event_queue,
                    parent_turn=parent_turn,
                    provider_key=provider_key,
                    progress=sub_progress,
                    surface=surface,
                    trigger=trigger,
                    error=f"sub-agent exceeded its {timeout_seconds}s wall-clock cap",
                    error_kind="timeout",
                )
                return SubagentResult(
                    text="(sub-agent timed out)",
                    turns_used=partial_turns,
                    tokens_used=partial_tokens,
                    rejected_tools=rejected,
                    timed_out=True,
                )
            except SubagentBudgetExceeded as exc:
                partial_turns = observed["rounds_used"]
                # Prefer the fractional wrapper's precise accounting when
                # available (it counts every token the sub-agent consumed,
                # including those attributed in the round that tripped the
                # cap); fall back to the on_progress snapshot otherwise.
                partial_tokens = observed["tokens_used"]
                if isinstance(effective_budget_manager, _FractionalBudgetManager):
                    partial_tokens = max(
                        partial_tokens, effective_budget_manager._subagent_used
                    )
                logger.warning(
                    "subagent_budget_exhausted_clean_exit",
                    agent_id=parent_turn.agent.id_str,
                    error=str(exc),
                    turns_used=partial_turns,
                    tokens_used=partial_tokens,
                )
                span.set_attribute("subagent.turns_used", partial_turns)
                span.set_attribute("subagent.tokens_used", partial_tokens)
                span.set_attribute("subagent.budget_exhausted", True)
                parent_turn.subagent_count += 1
                parent_turn.subagent_tokens += partial_tokens
                await _publish_subagent_failure(
                    event_queue=event_queue,
                    parent_turn=parent_turn,
                    provider_key=provider_key,
                    progress=sub_progress,
                    surface=surface,
                    trigger=trigger,
                    error=str(exc) or "sub-agent token budget exhausted",
                    error_kind="budget_exhausted",
                )
                return SubagentResult(
                    text="(sub-agent budget exhausted)",
                    turns_used=partial_turns,
                    tokens_used=partial_tokens,
                    rejected_tools=rejected,
                    timed_out=False,
                )

            # Happy path: record the final turn-count and token-count on
            # the span before it closes.
            tokens_used = loop.input_tokens + loop.output_tokens
            span.set_attribute("subagent.turns_used", loop.rounds_used)
            span.set_attribute("subagent.tokens_used", tokens_used)

    parent_turn.subagent_count += 1
    parent_turn.subagent_tokens += tokens_used
    notes = ""
    if rejected:
        notes = f"rejected_tools: {', '.join(rejected)}"
    await publish_phase_completed(
        event_queue=event_queue,
        agent=parent_turn.agent,
        turn_id=parent_turn.turn_id,
        conversation_key=parent_turn.stored_conversation_key,
        iteration=parent_turn.iteration,
        phase="subagent",
        provider_key=provider_key,
        loop=loop,
        decision="",  # sub-agents have no structured decision
        notes=notes,
        tools_available=list(surface.names),
        trigger=trigger,
    )
    return SubagentResult(
        text=loop.text,
        turns_used=loop.rounds_used,
        tokens_used=tokens_used,
        rejected_tools=rejected,
        timed_out=False,
        model=loop.model,
    )


@dataclass
class SubagentTask:
    """One task in a batched :func:`spawn_subagent_batch` call.

    Each task is independent (its own ``task_prompt`` / ``system_prompt``
    / optional per-task ``max_turns``) but shares the batch-level
    tool allowlist, budget slice, and aggregate timeout. The tool
    allowlist is one set applied to every child -- the planner does
    not get to grant heterogeneous capabilities in a single call.
    """

    task_prompt: str
    system_prompt: str
    max_turns: int | None = None


async def spawn_subagent_batch(
    *,
    parent_turn: TurnContext,
    tasks: list[SubagentTask],
    tool_names: list[str],
    parent_tool_names: list[str],
    provider: LLMProvider,
    provider_key: str,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    agent_context: AgentContext,
    event_queue: EventQueue,
    max_turns_cap: int = 20,
    timeout_seconds: float = 120.0,
    batch_timeout_seconds: float = 120.0,
    max_parallel: int = 3,
    min_per_child_tokens: int = 500,
    budget_fraction: float = 0.2,
    budget_manager: Any = None,
    observability: Any = None,
    token_usage_repo: Any = None,
    validators: list[Any] | None = None,
    prompt_skill_registry: Any = None,
) -> list[SubagentResult]:
    """Spawn ``len(tasks)`` sub-agents concurrently and return their
    results in input order.

    Design choices:

    - **One shared budget wrapper for the entire batch.** The total
      slice is ``budget_fraction * parent_remaining`` (computed
      once); every child consumes from the *same*
      :class:`_FractionalBudgetManager`. Whoever calls
      :meth:`~_FractionalBudgetManager.consume` first wins; later
      children see less budget remaining. This deliberately makes
      children compete -- it is the alternative (separate per-child
      wrappers each seeing the parent's full remaining) that
      over-allocates by ``N``.
    - **One aggregate timeout, not N.** The whole ``asyncio.gather``
      is wrapped in a single :func:`asyncio.wait_for` with
      ``batch_timeout_seconds``. A pathological 20-child batch
      cannot exceed that wall-clock cap even though each
      :func:`spawn_subagent` has its own per-child ``timeout_seconds``.
    - **No delegation-chain race.** All children share the one
      ``parent_turn`` object, but sub-agents are leaf-only -- they
      never spawn further sub-agents or contact colleagues, so they
      never extend ``parent_turn.delegation_chain``. ``spawn_subagent``
      only *reads* the chain (to build the surface) and only mutates
      the integer counters ``subagent_count`` / ``subagent_tokens``,
      whose ``+=`` is atomic under asyncio's single-threaded
      cooperative scheduling (no await between read and write). So
      concurrent children are safe without an explicit snapshot.
    - **Same allowlist applies to all children.** One ``tool_names``
      list applied uniformly. If you need heterogeneous tool surfaces
      per child, make separate calls.
    - **Failure isolation.** ``return_exceptions=True`` on
      ``gather``; a raising child produces a
      :class:`SubagentResult` with non-empty ``error`` and zero
      counters, and siblings keep running.
    """
    if not tasks:
        return []

    if min_per_child_tokens > 0 and budget_manager is not None:
        total_cap = _subagent_budget_cap(
            budget_manager=budget_manager,
            parent_agent_id=parent_turn.agent.id_str,
            fraction=budget_fraction,
        )
        if total_cap > 0 and total_cap < min_per_child_tokens * len(tasks):
            # Surface as a synthetic error per child rather than
            # raising -- the planner gets back the same result-list
            # shape and can react accordingly.
            err = (
                f"batch rejected: total budget slice {total_cap} tokens < "
                f"min_per_child_tokens ({min_per_child_tokens}) * "
                f"{len(tasks)} tasks"
            )
            return [
                SubagentResult(
                    text="",
                    turns_used=0,
                    tokens_used=0,
                    rejected_tools=[],
                    timed_out=False,
                    error=err[:500],
                )
                for _ in tasks
            ]

    # One shared budget wrapper for the entire batch (see docstring).
    effective_budget_manager: Any = budget_manager
    if budget_manager is not None:
        total_cap = _subagent_budget_cap(
            budget_manager=budget_manager,
            parent_agent_id=parent_turn.agent.id_str,
            fraction=budget_fraction,
        )
        if total_cap > 0:
            effective_budget_manager = _FractionalBudgetManager(
                budget_manager, max_subagent_tokens=total_cap
            )

    semaphore = asyncio.Semaphore(max(1, max_parallel))

    # Per-task result slots, filled as each child settles.
    #
    # The aggregate timeout below cancels the ``gather``, which discards
    # the return value of every child — including the ones that had
    # already FINISHED. Reporting those as timed-out with empty text
    # throws away work that completed and was paid for: the tokens are
    # spent either way, and the planner is told a child produced nothing
    # when it produced an answer. A slot each is what lets the timeout
    # path tell the two apart.
    settled: list[SubagentResult | None] = [None] * len(tasks)

    async def _run_one(index: int, task: SubagentTask) -> SubagentResult:
        async with semaphore:
            try:
                result = await spawn_subagent(
                    parent_turn=parent_turn,
                    task_prompt=task.task_prompt,
                    system_prompt=task.system_prompt,
                    tool_names=tool_names,
                    parent_tool_names=parent_tool_names,
                    provider=provider,
                    provider_key=provider_key,
                    registry=registry,
                    role_mcp_tools=role_mcp_tools,
                    agent_context=agent_context,
                    event_queue=event_queue,
                    max_turns_cap=max_turns_cap,
                    timeout_seconds=timeout_seconds,
                    max_turns_request=task.max_turns,
                    # ``budget_fraction=0`` disables the per-child
                    # cap inside ``spawn_subagent`` -- the batch-level
                    # ``_FractionalBudgetManager`` already enforces the
                    # shared cap, so re-wrapping per child would just
                    # nest a redundant (looser) wrapper.
                    budget_fraction=0,
                    budget_manager=effective_budget_manager,
                    observability=observability,
                    token_usage_repo=token_usage_repo,
                    validators=validators,
                    prompt_skill_registry=prompt_skill_registry,
                )
            except Exception as exc:
                logger.exception("subagent_batch_child_failed")
                result = SubagentResult(
                    text="",
                    turns_used=0,
                    tokens_used=0,
                    rejected_tools=[],
                    timed_out=isinstance(exc, TimeoutError),
                    error=str(exc)[:500],
                )
            settled[index] = result
            return result

    try:
        results = await asyncio.wait_for(
            asyncio.gather(*(_run_one(i, task) for i, task in enumerate(tasks))),
            timeout=batch_timeout_seconds,
        )
    except TimeoutError:
        # Aggregate-timeout fallback: surface as one error result per
        # task so the planner sees the structured failure shape. Fall
        # through to the shared publish + return below so a timed-out
        # batch still emits a ``SubagentBatched`` event (all failures).
        logger.warning(
            "subagent_batch_aggregate_timeout",
            batch_timeout_seconds=batch_timeout_seconds,
            task_count=len(tasks),
        )
        # Children that already settled keep their real result; only the
        # ones still running when the deadline landed are reported as
        # timed out. Overwriting the lot would discard answers that were
        # produced and paid for.
        results = [
            done
            if (done := settled[index]) is not None
            else SubagentResult(
                text="",
                turns_used=0,
                tokens_used=0,
                rejected_tools=[],
                timed_out=True,
                error=f"batch wall-clock exceeded {batch_timeout_seconds}s",
            )
            for index in range(len(tasks))
        ]

    # Publish the batch summary event for both the normal and the
    # aggregate-timeout path.  Best-effort: telemetry must never abort
    # a turn.
    try:
        from crewlet.events.types import SubagentBatched

        successes = sum(1 for r in results if not r.error and not r.timed_out)
        failures = len(results) - successes
        await event_queue.publish(
            "crewlet.events.subagent_batched",
            SubagentBatched(
                source=parent_turn.agent.role_name,
                parent_handle=parent_turn.agent.handle,
                task_count=len(tasks),
                successes=successes,
                failures=failures,
                total_tokens=sum(r.tokens_used for r in results),
            ),
        )
    except Exception:
        logger.exception("subagent_batched_event_publish_failed")

    return list(results)


__all__ = [
    "SubagentResult",
    "SubagentTask",
    "spawn_subagent",
    "spawn_subagent_batch",
]
