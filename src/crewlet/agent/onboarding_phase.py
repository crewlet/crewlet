"""Dedicated first-turn onboarding phase (runs before Plan).

First-turn onboarding — read the team's ``Onboarding`` pages, capture
conventions via ``reflect_and_persist``, then call ``mark_onboarded`` — used
to happen *inside* the Plan phase, driven by a hint injected into the Plan
prompt. On a genuine first turn that consumed the Plan round budget on
onboarding + recon and could starve ``submit_plan`` entirely.

This module runs onboarding as its **own** phase before Plan, with its own
round budget (``turn_engine.onboarding_max_tool_rounds``), so onboarding never
competes with planning. The surface is the onboarding builtins
(``reflect_and_persist`` / ``mark_onboarded`` / ``load_tool_skill``) plus the
discovery meta-tools (``activate_tool`` / ``list_mcp_server_tools``) so the
agent can locate its knowledge-base tools — the same discovery model as Plan,
but on a separate budget. The surface is relabelled ``phase="onboarding"``,
which :class:`~crewlet.tools.surface.ToolSurface` treats as a discovery phase
(it must, or ``activate`` silently refuses and the agent can never reach its
Confluence tools). No required-skill guard: onboarding is a fixed read →
persist → mark workflow and the hint is its own guidance, so the
load-before-use tax would only burn rounds.

The base round cap is governed by the same round-cap **extension judge** as
Plan/Execute: when ``onboarding_max_tool_rounds`` is exhausted and the agent is
still making progress, the judge grants more rounds up to
``onboarding_max_tool_rounds_ceiling`` rather than cutting the pass off
mid-read. There is no onboarding rescue path — a ``rescue``/ceiling outcome
just ends the pass unmarked (it retries next turn) — so the judge is additive.

**Run-once semantics.** Onboarding must run until it marks, then never again
for the same org chain. Four mechanisms guarantee that:

- the durable marker (``agent_onboarding_markers``) is read **tri-state**:
  ``True`` skips, ``False`` runs the pass, and ``None`` (lookup failed —
  state unknown) SKIPS this turn and retries the check next turn.
  Collapsing failures into "not onboarded" would re-run full passes
  for already-marked agents on transient DB errors.
- a **process-local latch** (``AgentInstance.onboarded_chain_hash``) is set
  the moment a pass marks (or a read confirms the marker) so no later read
  flake can re-fire onboarding within this process. The latch is keyed by
  chain hash, so a live org restructure still re-onboards by design.
- a **single-flight lock** (``AgentInstance.onboarding_lock``) holds any
  concurrent turn at the gate while a pass is running, then lets it skip via
  the latch. Turns for one agent are normally serialized by
  ``start_working``, but the sandbox busy-state transitions
  (``await_sandbox`` / ``resume_from_sandbox``) can free the agent while an
  earlier turn is still mid-flight — without the lock, two overlapping turns
  both read "unmarked" and run duplicate passes concurrently.
- a **cross-process DB lease** (``OnboardingMarkerStore.try_claim_pass``,
  TTL-bounded): the latch + lock are process-local, and agent inboxes are
  Shared subscriptions — during a rolling restart two engines can each run
  a turn for the same un-onboarded agent. The lease makes exactly one of
  them run the pass; the loser skips.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.extension import DeferredJudgeEvent, emit_deferred, maybe_extend
from crewlet.agent.llm_loop import (
    LoopResult,
    publish_phase_completed,
    publish_phase_started,
    run_tool_loop,
)
from crewlet.agent.prompts import build_onboarding_prompt
from crewlet.agent.tool_discovery import (
    build_activate_tool,
    build_list_mcp_server_tools,
)
from crewlet.agent.turn_context import TurnContext
from crewlet.events.types import describe_trigger
from crewlet.learning.onboarding import (
    MARK_ONBOARDED_TOOL,
    build_onboarding_hint,
    compute_chain_hash,
    is_onboarded,
)
from crewlet.org.models import Organization, Role
from crewlet.providers.llm.protocol import LLMProvider, Message
from crewlet.queue.protocol import EventQueue
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry
from crewlet.tools.surface import ToolSurface, merge_registry_and_mcp

logger = get_logger("agent.onboarding")

_ONBOARDING_ALWAYS_ON = ["reflect_and_persist", MARK_ONBOARDED_TOOL, "load_tool_skill"]

# Cross-process pass-lease TTL. The pass is bounded by the onboarding round
# budget (base 10, judge-extended up to the ceiling of 20 rounds — minutes of
# LLM calls, ~20-30s each worst case, so ≈10 min at the ceiling); 15 min
# covers that with slack for slow providers. A crashed claimant blocks
# re-onboarding for at most this long before the lease expires.
_ONBOARDING_CLAIM_TTL_S = 900.0


async def run_onboarding_phase(
    *,
    turn: TurnContext,
    provider: LLMProvider,
    provider_key: str,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    event_queue: EventQueue,
    agent_context: AgentContext,
    max_rounds: int,
    judge_provider: LLMProvider | None = None,
    judge_provider_key: str = "",
    extension_enabled: bool = False,
    extension_ceiling: int = 0,
    extension_round_step: int = 8,
    budget_manager: Any = None,
    observability: Any = None,
    token_usage_repo: Any = None,
    validators: list[Any] | None = None,
    on_start: Callable[[], Awaitable[None]] | None = None,
) -> bool:
    """Run the dedicated first-turn onboarding pass.

    Returns ``True`` if the pass actually ran (the agent was unmarked and the
    onboarding machinery is wired), ``False`` when it was skipped — already
    onboarded, no marker store / ``mark_onboarded`` tool, no org/role on the
    context, marker state unknown (lookup failed — retry next turn), or the
    budget is disabled. The caller sets ``turn.onboarding_ran`` from the
    result so the Plan-prompt hint is suppressed for the turn.

    ``on_start`` fires once the pass has cleared EVERY skip gate (including
    the DB marker read and the cross-process lease) and is about to do real
    work. Callers that want to announce the pass — the turn engine swaps the
    Slack working status to "is getting up to speed…" — must hook here
    rather than before the call: the overwhelmingly common outcome is a skip
    for an already-onboarded agent, and announcing that would be wrong every
    turn but the first.
    """
    if max_rounds <= 0:
        return False
    org = getattr(agent_context, "org", None)
    role_name = getattr(agent_context, "role", "") or ""
    if org is None or not role_name:
        return False
    try:
        role_obj = org.get_role(role_name)
    except Exception:
        logger.exception("onboarding_role_lookup_failed", role=role_name)
        return False
    if role_obj is None:
        return False
    # Onboarding can only complete if it can be marked; without the store /
    # tool the pass would run every turn forever. Skip cleanly.
    marker_store = getattr(agent_context, "onboarding_marker_store", None)
    if marker_store is None or registry.get(MARK_ONBOARDED_TOOL) is None:
        return False
    try:
        chain_hash = compute_chain_hash(role_obj, org)
    except Exception:
        logger.exception("onboarding_chain_hash_failed", role=role_name)
        return False
    # Process-local latch (fast path, re-checked under the lock): once this
    # process has CONFIRMED the agent is onboarded for this chain (a marker
    # read, or a successful mark), no DB read can re-fire the pass — the
    # marker store is best-effort, and a transient lookup failure must not
    # re-run a whole onboarding pass for an already-marked agent.
    if turn.agent.onboarded_chain_hash == chain_hash:
        return False

    # Single-flight: a concurrent turn (possible when the sandbox busy-state
    # transitions freed the agent mid-turn) waits here while a pass runs,
    # then re-checks the latch and skips instead of duplicating the pass.
    async with turn.agent.onboarding_lock:
        if turn.agent.onboarded_chain_hash == chain_hash:
            return False
        try:
            already = await is_onboarded(
                marker_store=marker_store,
                agent_id=agent_context.agent_id,
                expected_chain_hash=chain_hash,
            )
        except Exception:
            logger.exception("onboarding_is_onboarded_check_failed", role=role_name)
            already = None
        if already is True:
            turn.agent.onboarded_chain_hash = chain_hash
            return False
        if already is None:
            # Unknown (lookup failed): do NOT run the pass. The agent may
            # well be marked already, and a spurious repeat onboarding is
            # strictly worse than retrying the check next turn.
            logger.warning(
                "onboarding_state_unknown_skipping",
                agent=turn.agent.handle,
                role=role_name,
            )
            return False

        # Cross-process single-flight (the latch + lock above are
        # process-local): agent inboxes are Shared subscriptions, so during
        # a rolling restart TWO engines can each run a turn for this
        # un-onboarded agent. The DB lease makes exactly one of them run
        # the pass; the loser skips and the winner's mark suppresses every
        # later attempt.
        if not await marker_store.try_claim_pass(
            agent_id=agent_context.agent_id,
            ttl_seconds=_ONBOARDING_CLAIM_TTL_S,
        ):
            logger.info(
                "onboarding_claimed_elsewhere_skipping",
                agent=turn.agent.handle,
                role=role_name,
            )
            return False
        try:
            if on_start is not None:
                await on_start()
            return await _run_pass(
                turn=turn,
                provider=provider,
                provider_key=provider_key,
                registry=registry,
                role_mcp_tools=role_mcp_tools,
                event_queue=event_queue,
                agent_context=agent_context,
                max_rounds=max_rounds,
                judge_provider=judge_provider,
                judge_provider_key=judge_provider_key,
                extension_enabled=extension_enabled,
                extension_ceiling=extension_ceiling,
                extension_round_step=extension_round_step,
                budget_manager=budget_manager,
                observability=observability,
                token_usage_repo=token_usage_repo,
                validators=validators,
                role_obj=role_obj,
                org=org,
                chain_hash=chain_hash,
            )
        finally:
            # A marked pass already cleared the lease (mark() resets it);
            # an unmarked / crashed pass must not hold re-onboarding
            # hostage until the TTL, so release explicitly either way.
            await marker_store.release_claim(agent_context.agent_id)


async def _run_pass(
    *,
    turn: TurnContext,
    provider: LLMProvider,
    provider_key: str,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    event_queue: EventQueue,
    agent_context: AgentContext,
    max_rounds: int,
    judge_provider: LLMProvider | None,
    judge_provider_key: str,
    extension_enabled: bool,
    extension_ceiling: int,
    extension_round_step: int,
    budget_manager: Any,
    observability: Any,
    token_usage_repo: Any,
    validators: list[Any] | None,
    role_obj: Role,
    org: Organization,
    chain_hash: str,
) -> bool:
    """The actual onboarding LLM pass. Runs under the single-flight lock."""
    hint = build_onboarding_hint(role_obj, org)
    trigger = describe_trigger(turn.trigger_event)
    # Pre-Plan: label this iteration 0 so it groups under the turn before the
    # first Plan (which runs at iteration 1) on the dashboard.
    iteration = 0
    await publish_phase_started(
        event_queue=event_queue,
        agent=turn.agent,
        turn_id=turn.turn_id,
        iteration=iteration,
        phase="onboarding",
        trigger=trigger,
    )

    # Discovery surface: onboarding builtins always-on + the activate /
    # list-servers meta-tools so the agent can find its knowledge-base tools.
    catalogue_tools = merge_registry_and_mcp(registry, role_mcp_tools)
    surface_holder: list[Any] = [None]
    meta_tools = [
        build_activate_tool(
            catalogue_tools,
            surface_holder,
            phase="onboarding",
            event_queue=event_queue,
            agent_id=turn.agent.id_str,
            agent_role=turn.agent.role_name,
            turn_id=turn.turn_id,
            iteration=iteration,
        ),
        build_list_mcp_server_tools(
            role_mcp_tools,
            availability_filter=turn.availability_set,
        ),
    ]
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools,
        tools_needed=[],
        always_on=_ONBOARDING_ALWAYS_ON,
        meta_tools=meta_tools,
        availability_filter=turn.availability_set,
    )
    surface_holder[0] = surface
    surface.phase = "onboarding"
    # Deliberately NO surface.skill_guard — see module docstring.

    system_prompt = build_onboarding_prompt(
        turn.agent.definition,
        onboarding_hint=hint,
        tool_catalogue=surface.catalogue_text(),
    )
    messages = [
        Message(role="system", content=system_prompt),
        Message(role="user", content="Complete your onboarding now."),
    ]

    loop: LoopResult = await run_tool_loop(
        provider=provider,
        messages=messages,
        surface=surface,
        context=agent_context,
        agent=turn.agent,
        max_rounds=max_rounds,
        event_queue=event_queue,
        budget_manager=budget_manager,
        observability=observability,
        token_usage_repo=token_usage_repo,
        validators=validators,
        provider_key=provider_key,
        a2a_context=turn.a2a_context,
        terminate_after=[MARK_ONBOARDED_TOOL],
        turn_id=turn.turn_id,
        iteration=iteration,
        trigger=trigger,
    )

    # Round-cap extension judge (same mechanism as Plan/Execute): when the base
    # cap is exhausted and the agent is still making progress (reading a page,
    # persisting a convention), the judge grants more rounds up to the
    # onboarding ceiling instead of cutting the pass off mid-read. Extension
    # counters fold into ``loop`` so the phase event + token accounting see the
    # whole pass. There is no onboarding rescue path — a "rescue"/ceiling
    # outcome just ends the pass unmarked (it retries next turn), so the judge
    # is purely additive.
    total_rounds = loop.rounds_used
    deferred_judge_events: list[DeferredJudgeEvent] = []
    if extension_enabled and judge_provider is not None and loop.exhausted_rounds:
        while loop.exhausted_rounds and total_rounds < extension_ceiling:
            ext_loop, _decision = await maybe_extend(
                main_loop=loop,
                phase="onboarding",
                task_description=(
                    "First-turn onboarding: read the team's Onboarding pages, "
                    "persist conventions via reflect_and_persist, then call "
                    "mark_onboarded."
                ),
                plan_summary=hint,
                judge_provider=judge_provider,
                judge_provider_key=judge_provider_key,
                main_provider=provider,
                main_provider_key=provider_key,
                main_surface=surface,
                main_terminate_after=[MARK_ONBOARDED_TOOL],
                main_tool_choice=None,
                enabled=True,
                round_step=extension_round_step,
                rounds_remaining_under_ceiling=(extension_ceiling - total_rounds),
                context=agent_context,
                turn=turn,
                event_queue=event_queue,
                registry=registry,
                role_mcp_tools=role_mcp_tools,
                budget_manager=budget_manager,
                observability=observability,
                token_usage_repo=token_usage_repo,
                validators=validators,
                deferred_events=deferred_judge_events,
            )
            if ext_loop is None:
                break
            loop.tool_executions.extend(ext_loop.tool_executions)
            loop.input_tokens += ext_loop.input_tokens
            loop.output_tokens += ext_loop.output_tokens
            loop.rounds_used += ext_loop.rounds_used
            loop.exhausted_rounds = ext_loop.exhausted_rounds
            if ext_loop.text:
                loop.text = ext_loop.text
            total_rounds += ext_loop.rounds_used

    turn.input_tokens += loop.input_tokens
    turn.output_tokens += loop.output_tokens
    turn.model_keys["onboarding"] = loop.model or provider_key

    marked = any(
        e.get("name") == MARK_ONBOARDED_TOOL and e.get("success") is not False
        for e in loop.tool_executions
    )
    if marked:
        # Latch in-process so no later marker-store read flake (the store is
        # best-effort) can ever re-run onboarding for this chain.
        turn.agent.onboarded_chain_hash = chain_hash
    notes = "marked" if marked else "did not mark (will retry next turn)"
    await publish_phase_completed(
        event_queue=event_queue,
        agent=turn.agent,
        turn_id=turn.turn_id,
        iteration=iteration,
        phase="onboarding",
        provider_key=provider_key,
        loop=loop,
        decision="done" if marked else "",
        notes=notes,
        tools_available=sorted(surface.names),
        trigger=trigger,
    )
    # Flush judge events AFTER the host onboarding event so chronological
    # dashboards render onboarding -> judge(onboarding), not the reverse.
    await emit_deferred(deferred_judge_events)
    logger.info(
        "onboarding_phase_complete",
        agent=turn.agent.handle,
        turn_id=turn.turn_id,
        marked=marked,
        rounds=loop.rounds_used,
    )
    return True


__all__ = ["run_onboarding_phase"]
