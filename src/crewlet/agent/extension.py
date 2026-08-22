"""Per-phase round-cap extension judge.

When a Plan or Execute phase exhausts ``max_tool_rounds`` without
finishing, the existing rescue paths (``_rescue_submit_plan`` /
``_grace_summarize_execute``) force a one-shot wrap-up.  That throws
away in-progress work whenever the agent was actually close to done --
the cap is a static guess at how much room a turn needs.

The extension judge interposes between exhaustion and rescue: a cheap
LLM call inspects the tool log and decides:

- ``extend`` -- the agent is making meaningful progress; grant
  ``additional_rounds`` more rounds (bounded by the per-phase ceiling).
- ``rescue`` -- the agent is thrashing / stuck; fall through to the
  existing rescue path.

The judge runs on the role's ``llm_judge`` provider (resolved via
``phase_model.resolve_phase_chain`` with the standard fallback chain:
``llm_judge`` -> ``llm`` -> ``"default"`` -> first provider).  Typically
this is a cheap/fast model (Haiku-class) so the safety net does not
inflate per-turn cost.

Judge ``AgentPhaseCompleted`` events are emitted *deferred*: collected
during the host phase via ``DeferredJudgeEvent`` and flushed by the
host's runner AFTER its own ``publish_phase_completed`` call.  That way
dashboards sorting by timestamp see ``plan -> judge(plan) -> aux ->
execute -> judge(execute) ...`` instead of judges showing up before
the phase they belong to.  Events also carry ``host_phase`` /
``host_iteration`` so dashboards can group/render them as children.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict

from crewlet._logging import get_logger
from crewlet.agent.llm_loop import (
    LoopResult,
    publish_phase_completed,
    run_tool_loop,
    tag_phase_span,
)
from crewlet.agent.turn_context import TurnContext
from crewlet.events.types import describe_trigger
from crewlet.providers.llm.protocol import LLMProvider, Message
from crewlet.queue.protocol import EventQueue
from crewlet.telemetry import tracer
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.tools.surface import ToolSurface

logger = get_logger("agent.extension")


JudgePhase = Literal["plan", "execute", "onboarding"]


class ExtensionDecision(BaseModel):
    """Structured output from the extension judge."""

    model_config = ConfigDict(extra="forbid")

    decision: Literal["extend", "rescue"] = "rescue"
    reason: str = ""
    additional_rounds: int = 0
    """Number of additional rounds to grant when ``decision == "extend"``.
    Clamped by the caller to the configured step size and the remaining
    headroom under the per-phase ceiling.  Ignored when
    ``decision == "rescue"``."""


_JUDGE_TOOL_NAME = "submit_extension_decision"


@dataclass
class DeferredJudgeEvent:
    """A judge ``AgentPhaseCompleted`` event held back until the host
    phase has emitted its own event.

    The host phase runner builds an empty list, passes it down through
    ``maybe_extend`` / ``judge_extension``, and after its own
    ``publish_phase_completed`` call invokes :func:`emit_deferred` to
    flush the collected judge events in order.  Without this deferral
    chronological dashboards would render judges before the phase
    they belong to, since the judge fires DURING the host phase but
    the host event is published at the END.
    """

    publish: Callable[[], Awaitable[None]]


async def emit_deferred(events: list[DeferredJudgeEvent]) -> None:
    """Flush deferred judge events in order.  No-op when the list is
    empty (the common case: phase finished without exhausting)."""
    for ev in events:
        try:
            await ev.publish()
        except Exception:
            logger.exception("deferred_judge_publish_failed")


def _build_judge_tool() -> SimpleTool:
    """Structured-output tool that forces the judge into the decision enum."""

    async def _submit(params: dict[str, Any], context: AgentContext) -> ToolResult:
        try:
            ExtensionDecision.model_validate(params)
        except Exception as exc:
            return ToolResult(success=False, error=f"Invalid extension decision: {exc}")
        return ToolResult(success=True, output="extension decision submitted")

    return SimpleTool(
        name=_JUDGE_TOOL_NAME,
        description=(
            "Submit your judgement on whether to extend the agent's phase. "
            "Choose `extend` only when the tool log shows clear progress "
            "toward the task; choose `rescue` when the agent is looping, "
            "thrashing, or fundamentally stuck. `additional_rounds` (>=1) "
            "is the number of extra tool-call rounds to grant; the engine "
            "clamps it to the configured ceiling and step size."
        ),
        parameters=ExtensionDecision.model_json_schema(),
        fn=_submit,
    )


_SYSTEM_PROMPT = (
    "You are the extension judge for an agentic system. An agent has "
    "exhausted the tool-call round cap on its **{phase}** phase without "
    "completing the work. Decide: is the agent making meaningful "
    "progress (extend) or thrashing/stuck (rescue)?\n\n"
    "Default to `extend` when you see ANY of:\n"
    "- The agent just unblocked itself (e.g. error then tool activation) "
    "and has a clear next step in mind.\n"
    "- The last assistant message names a concrete next tool call "
    "or follow-up.\n"
    "- Previous rounds returned useful data the agent hasn't acted "
    "on yet.\n"
    "- The cap is small relative to the ceiling (plenty of headroom "
    "left).\n\n"
    "Choose `rescue` ONLY when you see clear thrashing:\n"
    "- Same tool called repeatedly with similar args AND similar "
    "results, no new info.\n"
    "- Tool errors the agent keeps repeating the same way.\n"
    "- Long assistant prose with no concrete action emerging.\n"
    "- Already delivered the task (final tool already called "
    "successfully) -- no point extending.\n\n"
    "Extension is cheap: it grants 1-{max_step} more rounds bounded "
    "by the ceiling. Rescue is final -- it forces a wrap-up and the "
    "phase ends. When in doubt and the agent shows ANY sign of "
    "purposeful action, extend with a small step (1-3 rounds).\n\n"
    "Call `submit_extension_decision` exactly once."
)


def _format_tool_log(executions: list[dict[str, Any]]) -> str:
    """Full tool log for the judge prompt.

    One line per execution: ``[N] name(args) -> success | error: <err>``.
    The complete history renders untruncated -- args and results in full,
    every round shown. Hiding the older rounds or clipping results is
    what lets a thrashing loop (the same failing call repeated) read as
    "purposeful" to the judge, so it sees everything.
    """
    if not executions:
        return "(no tool calls)"
    lines: list[str] = []
    for offset, exe in enumerate(executions):
        idx = offset + 1
        name = exe.get("name", "?")
        args = str(exe.get("arguments", ""))
        success = exe.get("success")
        result = str(exe.get("result", ""))
        if success is False or result.startswith("Unknown tool:"):
            outcome = f"error: {result}"
        else:
            outcome = f"ok: {result}"
        lines.append(f"[{idx}] {name}({args}) -> {outcome}")
    return "\n".join(lines)


def _build_judge_messages(
    *,
    phase: JudgePhase,
    task_description: str,
    plan_summary: str,
    executions: list[dict[str, Any]],
    last_assistant_text: str,
    rounds_used: int,
    max_step: int,
    rounds_remaining_under_ceiling: int,
) -> list[Message]:
    system = _SYSTEM_PROMPT.format(phase=phase, max_step=max_step)
    plan_block = f"\n\nPlan:\n{plan_summary.strip()}" if plan_summary.strip() else ""
    last_block = last_assistant_text or "(empty)"
    tool_log = _format_tool_log(executions)
    user = (
        f"Phase: {phase}\n"
        f"Rounds used in this run: {rounds_used}\n"
        f"Rounds remaining under the ceiling: {rounds_remaining_under_ceiling}\n"
        f"Step cap per extension: {max_step}\n\n"
        f"Task:\n{task_description.strip() or '(no description)'}"
        f"{plan_block}\n\n"
        f"Tool log (all rounds):\n{tool_log}\n\n"
        f"Last assistant message:\n{last_block}"
    )
    return [
        Message(role="system", content=system),
        Message(role="user", content=user),
    ]


def _parse_decision(messages: list[Message]) -> ExtensionDecision:
    """Walk the judge's messages and extract its `submit_extension_decision`.

    Falls back to a conservative ``rescue`` when the call isn't found or
    arguments fail validation -- the judge call is best-effort and must
    never push the host phase past exhaustion silently.
    """
    for msg in reversed(messages):
        if msg.role != "assistant" or not msg.tool_calls:
            continue
        for call in msg.tool_calls:
            if call.name == _JUDGE_TOOL_NAME:
                try:
                    return ExtensionDecision.model_validate(call.arguments)
                except Exception as exc:
                    logger.warning(
                        "extension_judge_parse_failed",
                        error=str(exc),
                        args=call.arguments,
                    )
                    return ExtensionDecision(decision="rescue", reason="parse_failed")
    return ExtensionDecision(decision="rescue", reason="no_decision_call")


async def judge_extension(
    *,
    phase: JudgePhase,
    task_description: str,
    plan_summary: str,
    executions: list[dict[str, Any]],
    last_assistant_text: str,
    rounds_used: int,
    max_step: int,
    rounds_remaining_under_ceiling: int,
    provider: LLMProvider,
    provider_key: str,
    context: AgentContext,
    turn: TurnContext,
    event_queue: EventQueue,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    budget_manager: Any = None,
    observability: Any = None,
    token_usage_repo: Any = None,
    validators: list[Any] | None = None,
    deferred_events: list[DeferredJudgeEvent] | None = None,
) -> ExtensionDecision:
    """Run the extension judge LLM call and return its decision.

    Best-effort: any failure (timeout, provider error, parse error) maps
    to a conservative ``rescue`` decision so the host phase falls
    through to its existing rescue path.

    When ``deferred_events`` is provided, the judge's
    ``AgentPhaseCompleted`` event is queued there instead of being
    published immediately -- the host phase runner flushes it via
    :func:`emit_deferred` after its own ``publish_phase_completed``
    fires, so chronological dashboards see the host phase first.
    """
    judge_tool = _build_judge_tool()
    surface = ToolSurface.for_judge(
        registry, role_mcp_tools, decision_tools=[judge_tool]
    )
    messages = _build_judge_messages(
        phase=phase,
        task_description=task_description,
        plan_summary=plan_summary,
        executions=executions,
        last_assistant_text=last_assistant_text,
        rounds_used=rounds_used,
        max_step=max_step,
        rounds_remaining_under_ceiling=rounds_remaining_under_ceiling,
    )
    # Dedicated OTel span so ``tag_phase_span`` tags judge attributes
    # (model, tokens, decision) on a child span rather than the parent
    # Plan/Execute span.  Span-entry attributes use a distinct ``judge.``
    # prefix so they don't collide with the ``phase.`` attributes
    # ``tag_phase_span`` sets later -- specifically, ``phase.rounds_used``
    # in tag_phase_span carries the JUDGE LLM call's own round count
    # (1-2), while the host phase's pre-judge count is captured here
    # as ``judge.host_rounds_used``.
    with tracer.start_as_current_span(
        "agent.turn.judge",
        attributes={
            "judge.host_phase": phase,
            "judge.host_rounds_used": rounds_used,
            "judge.host_rounds_remaining": rounds_remaining_under_ceiling,
            "turn.id": turn.turn_id,
        },
    ):
        try:
            loop = await run_tool_loop(
                provider=provider,
                messages=messages,
                surface=surface,
                context=context,
                agent=turn.agent,
                max_rounds=2,
                event_queue=event_queue,
                budget_manager=budget_manager,
                observability=observability,
                token_usage_repo=token_usage_repo,
                validators=validators,
                provider_key=provider_key,
                a2a_context=turn.a2a_context,
                terminate_after=[_JUDGE_TOOL_NAME],
                tool_choice="required",
                turn_id=turn.turn_id,
                iteration=turn.iteration,
                trigger=describe_trigger(turn.trigger_event),
            )
        except Exception:
            logger.exception("extension_judge_call_failed", phase=phase)
            return ExtensionDecision(decision="rescue", reason="judge_call_failed")

        turn.input_tokens += loop.input_tokens
        turn.output_tokens += loop.output_tokens

        decision = _parse_decision(loop.messages)

        notes = (
            f"{phase}: {decision.reason} (+{decision.additional_rounds} rounds)"
            if decision.decision == "extend"
            else f"{phase}: {decision.reason}"
        )
        host_iteration = turn.iteration

        # Tag the judge's OTel span EAGERLY while it's still the current
        # span -- if we let publish_phase_completed do it after the
        # publish (and the publish is deferred to fire after the host
        # phase's event), get_current_span() would return the host
        # phase span and we'd clobber its attributes.
        tag_phase_span(
            phase="judge",
            iteration=host_iteration,
            turn_id=turn.turn_id,
            rounds_used=loop.rounds_used,
            exhausted_rounds=loop.exhausted_rounds,
            rescue_fired=False,
            tool_executions_count=len(loop.tool_executions),
            decision=decision.decision,
            model=loop.model or provider_key,
            input_tokens=loop.input_tokens,
            output_tokens=loop.output_tokens,
        )

        async def _emit() -> None:
            await publish_phase_completed(
                event_queue=event_queue,
                agent=turn.agent,
                turn_id=turn.turn_id,
                conversation_key=turn.stored_conversation_key,
                iteration=host_iteration,
                phase="judge",
                provider_key=provider_key,
                loop=loop,
                decision=decision.decision,
                notes=notes,
                host_phase=phase,
                host_iteration=host_iteration,
                trigger=describe_trigger(turn.trigger_event),
                # Span was tagged eagerly above (we're still inside the
                # judge's span context here).  Skip tagging in publish
                # so the deferred call doesn't clobber the host phase
                # span when it eventually fires.
                tag_span=False,
            )

        if deferred_events is not None:
            deferred_events.append(DeferredJudgeEvent(publish=_emit))
        else:
            await _emit()
    return decision


async def maybe_extend(
    *,
    main_loop: LoopResult,
    phase: JudgePhase,
    task_description: str,
    plan_summary: str,
    # Judge provider (cheap; resolved via llm_judge -> llm -> default).
    judge_provider: LLMProvider,
    judge_provider_key: str,
    # Main provider (same one the host phase used; the extension is a
    # continuation of that conversation, so we keep the model stable).
    main_provider: LLMProvider,
    main_provider_key: str,
    main_surface: Any,
    main_terminate_after: list[str] | None,
    main_tool_choice: str | None,
    # Config.
    enabled: bool,
    round_step: int,
    rounds_remaining_under_ceiling: int,
    # Shared dependencies.
    context: AgentContext,
    turn: TurnContext,
    event_queue: EventQueue,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    budget_manager: Any = None,
    observability: Any = None,
    token_usage_repo: Any = None,
    validators: list[Any] | None = None,
    deferred_events: list[DeferredJudgeEvent] | None = None,
) -> tuple[LoopResult | None, ExtensionDecision]:
    """If the main loop exhausted rounds, judge and optionally extend.

    The caller is responsible for the outer "extend repeatedly until
    submit happens or ceiling reached" loop; this function handles
    exactly one judge + (optional) one extension run.

    Returns ``(extension_loop, decision)`` where ``extension_loop`` is
    ``None`` when no extension was granted (disabled, ceiling fully
    consumed, or judge said rescue), otherwise the
    :class:`~crewlet.agent.llm_loop.LoopResult` from continuing the
    main loop.  The decision is always returned so the caller can log
    why no extension was granted.

    Mutates ``main_loop.messages`` in place when extending: the nudge
    user message is appended and ``run_tool_loop`` extends with the
    extension's assistant/tool messages.  Doing so keeps the host
    phase's rescue check (e.g. ``_submit_plan_was_called``) accurate
    -- the rescue must see the full extended history, not just the
    pre-extension snippet.
    """
    if not enabled:
        return None, ExtensionDecision(decision="rescue", reason="extension_disabled")
    if rounds_remaining_under_ceiling <= 0:
        return None, ExtensionDecision(decision="rescue", reason="ceiling_reached")

    step = max(1, min(round_step, rounds_remaining_under_ceiling))
    decision = await judge_extension(
        phase=phase,
        task_description=task_description,
        plan_summary=plan_summary,
        executions=main_loop.tool_executions,
        last_assistant_text=main_loop.text,
        rounds_used=main_loop.rounds_used,
        max_step=step,
        rounds_remaining_under_ceiling=rounds_remaining_under_ceiling,
        provider=judge_provider,
        provider_key=judge_provider_key,
        context=context,
        turn=turn,
        event_queue=event_queue,
        registry=registry,
        role_mcp_tools=role_mcp_tools,
        budget_manager=budget_manager,
        observability=observability,
        token_usage_repo=token_usage_repo,
        validators=validators,
        deferred_events=deferred_events,
    )

    if decision.decision != "extend":
        return None, decision

    granted = max(1, min(decision.additional_rounds or step, step))
    logger.info(
        "extension_granted",
        phase=phase,
        granted_rounds=granted,
        rounds_used=main_loop.rounds_used,
        reason=decision.reason,
        turn_id=turn.turn_id,
    )

    # Phase-aware closing line: Plan exits by calling submit_plan
    # (structured output), Execute exits by returning text with no
    # tool calls.  Tailor the nudge so the LLM knows the right way to
    # finish before the extension cap hits again.
    if phase == "plan":
        finish_hint = (
            "If you cannot finish in this window, call ``submit_plan`` "
            "with whatever you have so far -- a partial plan is better "
            "than no plan."
        )
    else:  # execute
        finish_hint = (
            "If you cannot finish in this window, stop calling tools "
            "and return a short plain-text summary of what's done and "
            "what's outstanding."
        )

    main_loop.messages.append(
        Message(
            role="user",
            content=(
                f"You have been granted {granted} additional tool-call round(s) "
                f"because the extension judge sees you making progress.\n"
                f"Judge's reason: {decision.reason or '(none)'}\n"
                f"Use them efficiently to finish the task. {finish_hint}"
            ),
        )
    )

    try:
        extension_loop = await run_tool_loop(
            provider=main_provider,
            messages=main_loop.messages,
            surface=main_surface,
            context=context,
            agent=turn.agent,
            max_rounds=granted,
            event_queue=event_queue,
            budget_manager=budget_manager,
            observability=observability,
            token_usage_repo=token_usage_repo,
            validators=validators,
            provider_key=main_provider_key,
            a2a_context=turn.a2a_context,
            terminate_after=main_terminate_after,
            tool_choice=main_tool_choice,
            turn_id=turn.turn_id,
            iteration=turn.iteration,
            trigger=describe_trigger(turn.trigger_event),
        )
    except Exception:
        logger.exception("extension_run_failed", phase=phase)
        return None, ExtensionDecision(decision="rescue", reason="extension_run_failed")

    return extension_loop, decision


__all__ = [
    "DeferredJudgeEvent",
    "ExtensionDecision",
    "JudgePhase",
    "emit_deferred",
    "judge_extension",
    "maybe_extend",
]
