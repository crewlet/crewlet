"""Execute phase of the turn engine.

The Execute phase runs the plan. Its starting tool surface is
``plan.tools_needed`` unioned with the always-on set (default
``["load_tool_skill"]``), plus the ``activate_tool`` /
``list_mcp_server_tools`` discovery meta-tools so Execute can
discover and promote tools the planner missed. The system prompt
carries the same slim catalogue Plan sees (builtin tool names plus
MCP server names) so the executor knows what discovery surface is
available.

If the executor emits a tool call for a name not in its surface
and not in the role's catalogue, :func:`~crewlet.agent.llm_loop.execute_tool`
returns an ``Unknown tool: <name>`` result. After the loop
completes, :func:`run_execute_phase` inspects the tool-execution
trace and publishes an ``execute.missing_tool`` event for each such
name (signalling true plan incompleteness — not a name the executor
recovered by activating).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.extension import DeferredJudgeEvent, emit_deferred, maybe_extend
from crewlet.agent.llm_loop import (
    LoopResult,
    publish_phase_completed,
    publish_phase_started,
    run_tool_loop,
)
from crewlet.agent.plan import ExecutionPlan
from crewlet.agent.prompts import build_execute_prompt
from crewlet.agent.skills.guard import skill_guard_for_turn
from crewlet.agent.skills.models import Phase as SkillPhase
from crewlet.agent.tool_discovery import (
    build_activate_tool,
    build_list_mcp_server_tools,
)
from crewlet.agent.turn_context import TurnContext
from crewlet.events.types import ExecuteMissingTool, describe_trigger
from crewlet.providers.llm.protocol import LLMProvider, Message
from crewlet.queue.protocol import EventQueue
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.tools.surface import ToolSurface, merge_registry_and_mcp

logger = get_logger("agent.execute")


@dataclass
class ExecuteResumeState:
    """State needed to RESUME a suspended Execute tool-loop.

    Built by the engine from the persisted ``pending_sandbox_run.execute_state``
    plus the freshly-collected coding-agent result, and handed to
    :func:`run_execute_phase` via ``resume_from``. The loop continues with the
    persisted conversation, the sandbox result spliced in as the pending
    ``run_sandbox`` call's reply.
    """

    plan: Any
    result_content: str = ""
    messages: list[dict[str, Any]] = field(default_factory=list)
    pending_tool_call_id: str = ""
    pending_tool_name: str = ""
    active_tool_names: list[str] = field(default_factory=list)
    loaded_skill_keys: list[str] = field(default_factory=list)
    result_success: bool = True
    # Tool executions from the SUSPENDED portion of the loop (the run_sandbox
    # call that suspended, plus any calls before it). Replayed into the resumed
    # ExecuteResult so Review's ``## What Execute did`` evidence includes the
    # run_sandbox call — without this the resumed phase only logs its post-resume
    # calls and Review judges the (sandbox-delegated) work "fabricated" and loops
    # the turn forever.
    prior_tool_executions: list[dict[str, Any]] = field(default_factory=list)


@dataclass
class ExecuteResult:
    """Outcome of one Execute-phase run."""

    text: str = ""
    tool_executions: list[dict[str, Any]] = field(default_factory=list)
    missing_tools: list[str] = field(default_factory=list)
    exhausted_rounds: bool = False
    input_tokens: int = 0
    output_tokens: int = 0
    model: str = ""
    # Sandbox backend fields — populated only on the
    # sandboxed Execute path; ``backend`` stays "native" otherwise.
    backend: str = "native"
    sandbox_id: str = ""
    coding_agent: str = ""
    delivered_refs: list[str] = field(default_factory=list)
    cost_usd: float = 0.0
    changed_files: list[str] = field(default_factory=list)
    status: str = "done"
    """"done" | "detached" — "detached" when the executor called ``run_sandbox``
    and the Execute loop suspended awaiting the detached result. A mid-run
    clarification is handled entirely engine-side by the
    :class:`~crewlet.sandbox.coordinator.SandboxCoordinator` (it parks + resumes
    the suspended loop), so it never surfaces as an ExecuteResult status."""


def _detect_missing_tools(
    executions: list[dict[str, Any]], surface_names: set[str]
) -> list[str]:
    """Names appearing in tool executions that weren't in the surface.

    ``ToolSurface.lookup`` gatekeeps -- any tool call for a name
    outside the surface comes back as an ``Unknown tool: <name>``
    failure from :func:`~crewlet.agent.llm_loop.execute_tool` -- so
    membership in ``surface_names`` is the single source of truth.
    We deliberately do NOT also match on a ``result`` string prefix,
    because a legitimate surface tool could produce output that
    happens to start with ``"Unknown tool:"`` and that would flag a
    false positive.

    Names are de-duplicated while preserving first-seen order so a
    looping LLM that calls the same unknown tool N times produces one
    event, not N (and Review's missing-tools list stays readable).
    """
    missing: list[str] = []
    seen: set[str] = set()
    for exe in executions:
        name = exe.get("name", "")
        if not name or name in seen:
            continue
        if name not in surface_names:
            missing.append(name)
            seen.add(name)
    return missing


_EXECUTE_GRACE_DIRECTIVE = (
    "You've hit the tool-round cap before producing a complete "
    "summary.  Stop using tools right now.  In plain text, give a "
    "concise final wrap-up:\n\n"
    "1. What you actually accomplished this turn.\n"
    "2. What's still outstanding (if anything).\n"
    "3. Any blockers or partial results worth carrying forward.\n\n"
    "Do not call any tools in your response."
)


async def _grace_summarize_execute(
    *,
    provider: LLMProvider,
    provider_key: str,
    base_messages: list[Message],
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    agent_context: AgentContext,
    turn: TurnContext,
    event_queue: EventQueue,
    budget_manager: Any,
    observability: Any,
    token_usage_repo: Any,
    validators: list[Any] | None,
) -> LoopResult | None:
    """One-shot wrap-up call when the Execute loop exhausts its round
    cap without a clean stop.

    Builds a no-tool surface and nudges the LLM to produce a plain-
    text summary of work done so far.  Reuses
    :func:`~crewlet.agent.llm_loop.run_tool_loop` so budget /
    token-usage / prompt.size bookkeeping happen normally.  Best-
    effort: failures return ``None`` and the caller falls back to
    whatever ``loop.text`` carried.
    """
    rescue_surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools,
        tools_needed=[],
        always_on=[],
        expose_catalogue=False,
    )
    rescue_messages = list(base_messages) + [
        Message(role="user", content=_EXECUTE_GRACE_DIRECTIVE)
    ]
    logger.info(
        "execute_grace_invoked",
        agent=turn.agent.handle,
        turn_id=turn.turn_id,
    )
    try:
        rescue_loop = await run_tool_loop(
            provider=provider,
            messages=rescue_messages,
            surface=rescue_surface,
            context=agent_context,
            agent=turn.agent,
            # One round: the LLM has no tools to call, so a single
            # call returns plain text and the loop terminates
            # cleanly with ``exhausted=False``.
            max_rounds=1,
            event_queue=event_queue,
            budget_manager=budget_manager,
            observability=observability,
            token_usage_repo=token_usage_repo,
            validators=validators,
            provider_key=provider_key,
            a2a_context=turn.a2a_context,
            turn_id=turn.turn_id,
            iteration=turn.iteration,
            trigger=describe_trigger(turn.trigger_event),
        )
    except Exception:
        logger.exception("execute_grace_failed")
        return None
    return rescue_loop


async def _suspend_execute(
    *,
    turn: TurnContext,
    loop: LoopResult,
    surface: ToolSurface,
    provider_key: str,
    event_queue: EventQueue,
    pending_store: Any,
    trigger: dict[str, Any],
    full_tool_executions: list[dict[str, Any]],
    response_messages: list[Message] | None = None,
) -> ExecuteResult:
    """Persist a suspended Execute loop and return a ``detached`` result.

    Serializes the partial conversation + surface state onto the
    ``pending_sandbox_run`` row (created by the run_sandbox tool) so the
    coordinator can resume the loop when the detached run completes, then
    publishes the (suspended) Execute phase and ends the turn.

    ``full_tool_executions`` is the FULL tool history across any prior
    suspend/resume hops (the prepended ``prior_tool_executions`` + this
    segment's calls); it is what gets persisted + reported so Review's evidence
    log is complete. ``response_messages`` (a post-resume slice) scopes the
    published phase event to just this segment when the suspend happened on a
    *resumed* loop — the earlier segment is already its own checkpoint.
    """
    state = {
        # The FULL conversation is what resume replays — never the published
        # slice.
        "messages": [m.model_dump(mode="json") for m in loop.messages],
        "pending_tool_call_id": loop.pending_tool_call_id,
        "pending_tool_name": loop.pending_tool_name,
        "active_tool_names": sorted(surface.names),
        "loaded_skill_keys": (
            sorted(surface.skill_guard.loaded_keys) if surface.skill_guard else []
        ),
        "iteration": turn.iteration,
        "input_tokens": loop.input_tokens,
        "output_tokens": loop.output_tokens,
        # The full chain (prior hops + this segment) so the eventual resumed
        # ExecuteResult — and thus Review's evidence log — sees every call,
        # even across multiple suspend/resume hops.
        "tool_executions": full_tool_executions,
    }
    sandbox_id = (loop.suspend_payload or {}).get("sandbox_id", "")
    if pending_store is not None:
        try:
            await pending_store.save_execute_state(turn.turn_id, state)
        except Exception:
            logger.exception("execute_suspend_persist_failed", turn_id=turn.turn_id)
    turn.input_tokens += loop.input_tokens
    turn.output_tokens += loop.output_tokens
    turn.model_keys["execute"] = loop.model or provider_key
    await publish_phase_completed(
        event_queue=event_queue,
        agent=turn.agent,
        turn_id=turn.turn_id,
        iteration=turn.iteration,
        phase="execute",
        provider_key=provider_key,
        loop=loop,
        decision="",
        notes="suspended: run_sandbox (awaiting detached result)",
        tools_available=sorted(surface.names),
        trigger=trigger,
        response_messages=response_messages,
    )
    return ExecuteResult(
        text="(sandbox coding job launched, running detached)",
        tool_executions=full_tool_executions,
        input_tokens=loop.input_tokens,
        output_tokens=loop.output_tokens,
        model=loop.model,
        backend="sandbox",
        sandbox_id=sandbox_id,
        status="detached",
    )


async def run_execute_phase(
    *,
    turn: TurnContext,
    plan: ExecutionPlan,
    provider: LLMProvider,
    provider_key: str,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    always_on: list[str],
    event_queue: EventQueue,
    agent_context: AgentContext,
    max_rounds: int = 20,
    budget_manager: Any = None,
    observability: Any = None,
    token_usage_repo: Any = None,
    validators: list[Any] | None = None,
    relevant_knowledge_block: str = "",
    phantom_tools: list[str] | None = None,
    prompt_skill_registry: Any = None,
    judge_provider: LLMProvider | None = None,
    judge_provider_key: str = "",
    extension_enabled: bool = False,
    extension_ceiling: int = 0,
    extension_round_step: int = 8,
    pending_store: Any = None,
    resume_from: ExecuteResumeState | None = None,
) -> ExecuteResult:
    """Execute the plan.

    Builds an Execute-phase :class:`~crewlet.tools.surface.ToolSurface`
    from ``plan.tools_needed`` unioned with ``always_on``; drives it
    through :func:`~crewlet.agent.llm_loop.run_tool_loop`. Detects
    missing-tool calls and publishes ``execute.missing_tool`` events
    for each.

    ``relevant_knowledge_block`` is the post-Plan relevant-knowledge
    re-fetch (see
    :func:`crewlet.agent.plan.fetch_post_plan_relevant_knowledge`),
    resolved by the TurnEngine between Plan and Execute.  Non-empty
    only on thin-trigger turns where the Plan-phase prefetch was
    gated off; injected into the Execute system prompt as a
    ``## Relevant knowledge`` section.
    """
    trigger = describe_trigger(turn.trigger_event)
    await publish_phase_started(
        event_queue=event_queue,
        agent=turn.agent,
        turn_id=turn.turn_id,
        iteration=turn.iteration,
        phase="execute",
        trigger=trigger,
    )

    # Merge once via the canonical helper so the activate closure's
    # catalogue_map and the surface's _catalogue_tools agree on which
    # Tool instance backs each name (MCP overrides global for collisions).
    catalogue_tools = merge_registry_and_mcp(registry, role_mcp_tools)
    surface_holder: list[Any] = [None]
    meta_tools: list[SimpleTool] = [
        build_activate_tool(
            catalogue_tools,
            surface_holder,
            phase="execute",
            event_queue=event_queue,
            agent_id=turn.agent.id_str,
            agent_role=turn.agent.role_name,
            turn_id=turn.turn_id,
            iteration=turn.iteration,
        ),
        build_list_mcp_server_tools(
            role_mcp_tools,
            availability_filter=turn.availability_set,
        ),
    ]
    surface = ToolSurface.for_execute(
        registry,
        role_mcp_tools,
        tools_needed=plan.tools_needed,
        always_on=always_on,
        meta_tools=meta_tools,
        availability_filter=turn.availability_set,
    )
    surface_holder[0] = surface
    # Required-skill guard: even when Plan loaded a required skill, the
    # body lives in Plan's message history -- this session starts fresh
    # and must load it again before calling the covered tools.  The
    # extension loop reuses this surface, so loads persist across
    # extensions; the grace wrap-up builds its own no-tool surface and
    # is unaffected.
    surface.skill_guard = skill_guard_for_turn(
        registry=prompt_skill_registry,
        phase=SkillPhase.EXECUTE,
        surface=surface,
        turn=turn,
        event_queue=event_queue,
    )

    counterparty_profile = (
        turn.plan_prefetch.counterparty_profile if turn.plan_prefetch else ""
    )
    # Skill-catalogue scope: pass the FULL role catalogue (sans meta
    # tools, which are tool-management plumbing and not skill subjects)
    # so skills for any tool the executor MIGHT activate are surfaced.
    # The previous "plan.tools_needed ∪ always_on" scope hid skills for
    # tools the executor recovers via activate_tool. ``catalogue_names``
    # already excludes meta-tools (they're tracked separately in the
    # surface's _tools list, not _catalogue_tools).
    skill_available_tools = sorted(surface.catalogue_names())
    system_prompt = build_execute_prompt(
        turn.agent.definition,
        plan_summary=plan.summary(),
        counterparty_profile=counterparty_profile,
        relevant_knowledge_block=relevant_knowledge_block,
        available_tools=skill_available_tools,
        tool_catalogue=surface.catalogue_text(),
        phantom_tools=phantom_tools,
        skill_registry=prompt_skill_registry,
    )
    if resume_from is not None:
        # Resume a suspended loop: continue the persisted
        # conversation with the sandbox result spliced in as the pending
        # run_sandbox call's reply. Replay the surface state (executor-
        # activated tools + skills loaded this session) so the model keeps the
        # exact tool surface it had, and the required-skill guard doesn't
        # re-block tools it already unlocked.
        messages = [Message.model_validate(m) for m in resume_from.messages]
        messages.append(
            Message(
                role="tool",
                content=resume_from.result_content
                or "(sandbox run produced no output)",
                tool_call_id=resume_from.pending_tool_call_id,
                name=resume_from.pending_tool_name or "run_sandbox",
            )
        )
        for nm in resume_from.active_tool_names:
            surface.activate(nm)
        if surface.skill_guard is not None and resume_from.loaded_skill_keys:
            surface.skill_guard.loaded_keys.update(resume_from.loaded_skill_keys)
    else:
        messages = [
            Message(role="system", content=system_prompt),
            Message(
                role="user",
                content=f"Task:\n{turn.task_description or '(no description)'}",
            ),
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
        turn_id=turn.turn_id,
        iteration=turn.iteration,
        trigger=trigger,
        allow_suspend=True,
    )

    # The published phase event for a RESUMED run shows only THIS segment (the
    # post-resume slice) — the pre-suspend segment is already its own published
    # checkpoint, so replaying it would duplicate that row.
    # ``resume_msgs`` is that slice; the FULL tool history (prior suspend/resume
    # hops + this segment) is reassembled separately for the ExecuteResult,
    # Review's evidence log, missing-tool detection, and re-suspend persistence.
    prior_tools = (
        list(resume_from.prior_tool_executions) if resume_from is not None else []
    )
    resume_msgs = (
        loop.messages[len(resume_from.messages) + 1 :]
        if resume_from is not None
        else None
    )

    # Suspend: the executor called run_sandbox; the loop left that call
    # unanswered. Persist the conversation + surface state for resume, end the
    # turn (status="detached"). The run_sandbox tool already published
    # SandboxRunStarted (the coordinator's busy gate); the coordinator resumes
    # this loop when the detached run completes.
    if loop.suspended:
        return await _suspend_execute(
            turn=turn,
            loop=loop,
            surface=surface,
            provider_key=provider_key,
            event_queue=event_queue,
            pending_store=pending_store,
            trigger=trigger,
            full_tool_executions=[*prior_tools, *loop.tool_executions],
            response_messages=resume_msgs,
        )

    # Extension judge: when the main loop exhausts its round cap, ask a
    # cheap judge LLM whether Execute is making progress.  If yes, grant
    # more rounds (bounded by the configured ceiling); if no, fall
    # through to the grace call.  Extensions chain -- after each
    # extended run we re-judge if rounds were again exhausted, up to
    # the ceiling.  Tool executions / token counts from extensions are
    # folded into the main ``loop`` accounting so missing-tool detection
    # and the merged AgentPhaseCompleted event see the full picture.
    total_rounds = loop.rounds_used
    deferred_judge_events: list[DeferredJudgeEvent] = []
    if extension_enabled and judge_provider is not None and loop.exhausted_rounds:
        while loop.exhausted_rounds and total_rounds < extension_ceiling:
            ext_loop, _decision = await maybe_extend(
                main_loop=loop,
                phase="execute",
                task_description=turn.task_description,
                plan_summary=plan.summary(),
                judge_provider=judge_provider,
                judge_provider_key=judge_provider_key,
                main_provider=provider,
                main_provider_key=provider_key,
                main_surface=surface,
                main_terminate_after=None,
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
            # Fold extension counters into the main loop so the
            # per-phase event and missing-tool detection see the full
            # picture -- the extension is conceptually part of the
            # same Execute run.
            loop.tool_executions.extend(ext_loop.tool_executions)
            loop.input_tokens += ext_loop.input_tokens
            loop.output_tokens += ext_loop.output_tokens
            loop.rounds_used += ext_loop.rounds_used
            loop.exhausted_rounds = ext_loop.exhausted_rounds
            if ext_loop.text:
                loop.text = ext_loop.text
            total_rounds += ext_loop.rounds_used

    # Execute grace call. When the main loop exhausts its
    # round cap, the ``loop.text`` we have is whatever the LLM
    # happened to emit in its last assistant message -- possibly
    # empty, possibly mid-action. Fire one no-tools call asking
    # for a plain-text wrap-up so Review has a clean artifact to
    # evaluate. Mirrors the Plan / Review rescue pattern.
    rescue_fired = False
    rescue_loop: LoopResult | None = None
    final_text = loop.text
    if loop.exhausted_rounds:
        rescue_fired = True
        rescue_loop = await _grace_summarize_execute(
            provider=provider,
            provider_key=provider_key,
            base_messages=loop.messages,
            registry=registry,
            role_mcp_tools=role_mcp_tools,
            agent_context=agent_context,
            turn=turn,
            event_queue=event_queue,
            budget_manager=budget_manager,
            observability=observability,
            token_usage_repo=token_usage_repo,
            validators=validators,
        )
        if rescue_loop is not None:
            # The grace call consumed tokens whether or not it
            # produced text -- account for them at the turn level
            # regardless. Only adopt its text as the artifact when
            # non-empty.
            turn.input_tokens += rescue_loop.input_tokens
            turn.output_tokens += rescue_loop.output_tokens
            if rescue_loop.text:
                final_text = rescue_loop.text

    # Compute the post-loop surface name set so tools the executor
    # discovered + activated mid-run (via ``activate_tool``) are NOT
    # flagged as missing — only true unknowns get an ``execute.missing_tool``
    # event. Meta-tools never count as missing in the trace check.
    # Post-loop surface includes meta-tools (added in for_execute) AND
    # any tool the executor activated mid-run — both legitimately part
    # of the executor's call surface — so they don't count as
    # missing-tool events.
    # Full tool history (prior suspend/resume hops + this segment) for the
    # evidence log, missing-tool detection, and the returned result.
    full_tool_executions = [*prior_tools, *loop.tool_executions]
    post_loop_surface_names = set(surface.names)
    missing = _detect_missing_tools(full_tool_executions, post_loop_surface_names)
    for name in missing:
        event = ExecuteMissingTool(
            source=turn.agent.role_name,
            agent_id=turn.agent.id_str,
            role=turn.agent.role_name,
            tool_name=name,
            plan_tools=list(plan.tools_needed),
        )
        try:
            await event_queue.publish(f"crewlet.events.{event.type}", event)
        except Exception:
            logger.exception("missing_tool_event_publish_failed")

    turn.input_tokens += loop.input_tokens
    turn.output_tokens += loop.output_tokens
    turn.model_keys["execute"] = loop.model or provider_key

    notes = ""
    if missing:
        notes = f"missing_tools: {', '.join(missing)}"
    await publish_phase_completed(
        event_queue=event_queue,
        agent=turn.agent,
        turn_id=turn.turn_id,
        iteration=turn.iteration,
        phase="execute",
        provider_key=provider_key,
        loop=loop,
        decision="",  # Execute has no structured decision
        notes=notes,
        # Post-loop names so dashboards see the surface as it ended,
        # including any tools the executor activated mid-run.
        tools_available=sorted(surface.names),
        rescue_fired=rescue_fired,
        rescue_loop=rescue_loop,
        trigger=trigger,
        response_messages=resume_msgs,
    )
    # Flush any judge events that fired during this phase.  Deferred
    # so chronological dashboards see the execute event first, then
    # its judge children.
    await emit_deferred(deferred_judge_events)

    return ExecuteResult(
        text=final_text,
        tool_executions=full_tool_executions,
        missing_tools=missing,
        exhausted_rounds=loop.exhausted_rounds,
        input_tokens=loop.input_tokens,
        output_tokens=loop.output_tokens,
        model=loop.model,
    )


__all__ = ["ExecuteResult", "ExecuteResumeState", "run_execute_phase"]
