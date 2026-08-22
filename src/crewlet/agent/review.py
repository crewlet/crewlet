"""Review phase of the turn engine.

Review decides what happens next with the artifact Execute produced:

- **done** -- artifact meets the success criteria; return it.
- **self_iterate** -- loop back to Plan with the reviewer's notes.

When the turn needs a colleague or manager (a capability gap, a
blocker, or a decision above the agent's authority) there is no
separate handoff decision: Review chooses ``self_iterate`` and the
note tells Plan to add an outreach step so Execute reaches the
colleague directly with its own Slack / Jira / Confluence / A2A
tools -- exactly how a human teammate asks for help.

Review's surface is deliberately minimal: no domain tools; a single
``submit_review`` structured-output tool that forces the decision
into a :class:`ReviewOutcome` enum.
"""

from __future__ import annotations

import json
from typing import Any, Literal

from pydantic import BaseModel

from crewlet._logging import get_logger
from crewlet.agent.execute import ExecuteResult
from crewlet.agent.iteration_log import format_tool_calls, render_iteration_ledger
from crewlet.agent.llm_loop import (
    publish_phase_completed,
    publish_phase_started,
    run_tool_loop,
)
from crewlet.agent.plan import PLAN_META_TOOL_NAMES, ExecutionPlan
from crewlet.agent.prompts import build_review_prompt
from crewlet.agent.turn_context import TurnContext
from crewlet.events.types import describe_trigger
from crewlet.providers.llm.protocol import LLMProvider, Message
from crewlet.queue.protocol import EventQueue
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry
from crewlet.tools.surface import ToolSurface

logger = get_logger("agent.review")

ReviewDecision = Literal["done", "self_iterate"]


class ReviewOutcome(BaseModel):
    """Structured output from the Review phase."""

    decision: ReviewDecision = "done"
    notes: str = ""
    """Short prose explanation, shown to the next Plan round when the
    decision is ``self_iterate``. When the turn is blocked and needs a
    colleague, the note tells Plan to add the outreach step so Execute
    reaches them directly with its own colleague-surface tools."""

    completed_work: str = ""
    """What already landed this turn and must not be repeated, in the
    reviewer's own words -- the semantic layer over the engine-built
    tool-call ledger (see :mod:`crewlet.agent.iteration_log`).

    The ledger records that ``slack_conversations_add_message`` fired;
    only the reviewer can say "the post landed and reads fine, it just
    omits the breakdown -- follow up in that thread rather than
    re-posting". Empty on ``done`` (nothing loops back) and empty
    whenever Review never chose ``self_iterate`` itself, which is why
    the ledger and not this field carries the guarantee."""

    final_artifact: str = ""
    """The artifact Review wants to return when decision is ``done``.
    If empty, the engine returns Execute's text."""


def _build_review_meta_tools() -> list[SimpleTool]:
    async def _submit_review(
        params: dict[str, Any], context: AgentContext
    ) -> ToolResult:
        try:
            ReviewOutcome.model_validate(params)
        except Exception as exc:
            return ToolResult(success=False, error=f"Invalid review: {exc}")
        return ToolResult(success=True, output="review submitted")

    return [
        SimpleTool(
            name="submit_review",
            description=(
                "Submit your review decision. Call exactly once per "
                "review. Decisions:\n"
                "- `done`: success criteria met; put the final text "
                "  in `final_artifact` (or leave empty to reuse "
                "  Execute's output).\n"
                "- `self_iterate`: not done -- loop back to Plan with "
                "  an actionable correction in `notes`, and set "
                "  `completed_work` to what ALREADY landed this turn "
                "  (especially external side effects: posts, comments, "
                "  status changes) so the next round adds to it instead "
                "  of firing it a second time.\n\n"
                "Choose `self_iterate` whenever the artifact is wrong "
                "or incomplete, a required `tools_needed` action tool "
                "was not actually called, or Execute narrated that it "
                "lacks a tool it needs (e.g. \"I don't have Jira "
                'access") -- tell Plan to re-list `tools_needed` with '
                "the missing tool so Execute gets it next pass.\n\n"
                "Need a colleague or manager -- blocked, lacking "
                "authority, or needing someone else's identity / "
                "credentials? Still `self_iterate`: put the ask in "
                "`notes` so Plan adds an outreach step and Execute "
                "reaches them directly with its own Slack / Jira / "
                "Confluence / A2A tools (mention them where the work "
                "lives; they reply asynchronously and that re-triggers "
                "you). Never hand a naked problem -- include what you "
                "tried, the options you see, and your recommendation."
            ),
            parameters=ReviewOutcome.model_json_schema(),
            fn=_submit_review,
        )
    ]


def _format_execute_tool_log(executions: list[dict[str, Any]]) -> str:
    """Evidence-only summary of Execute's tool calls for Review.

    Each line: ``- <tool>(<args>) → success | error: <error>``.
    Args and error text render in full -- no truncation, so Review sees
    exactly what each call did. Empty list returns ``"(none)"`` so
    Review sees an explicit "no action taken" signal rather than just
    an absent section.

    Thin wrapper over :func:`~crewlet.agent.iteration_log.format_tool_calls`
    at its no-truncation defaults.  The cross-iteration ledger shares
    that renderer with elision budgets applied; this single-iteration
    evidence log deliberately does not -- it is what Review judges
    delivery against, so it must stay verbatim.
    """
    return format_tool_calls(executions)


def _format_plan_tool_log(executions: list[dict[str, Any]]) -> str:
    """Plan-phase tool log for Review, with meta-tools filtered out.

    Same renderer as :func:`_format_execute_tool_log` after dropping the
    Plan-only meta-tools (see ``PLAN_META_TOOL_NAMES`` in
    ``crewlet.agent.plan``).  An all-meta or empty input yields
    ``"(none)"`` so Review sees the explicit signal that Plan did no
    externally visible work during recon.
    """
    return format_tool_calls(executions, skip_names=PLAN_META_TOOL_NAMES)


_REVIEW_RESCUE_DIRECTIVE = (
    "Your previous turns ran out without calling `submit_review`.  "
    "Decide now: did Execute's artifact meet the success criteria?\n\n"
    '- If yes → `decision="done"` (set `final_artifact` or leave '
    "empty to reuse Execute's text).\n"
    '- Otherwise → `decision="self_iterate"` with a concrete '
    "actionable correction in `notes` (if you are blocked and need "
    "a colleague, say so in the note so Plan adds the outreach "
    "step).\n\n"
    "Your next call must be `submit_review`.  No further reasoning "
    "rounds -- your tool surface for this rescue is constrained to "
    "`submit_review` only."
)


def _submit_review_was_called(messages: list[Message]) -> bool:
    for msg in reversed(messages):
        if msg.role == "assistant" and msg.tool_calls:
            for call in msg.tool_calls:
                if call.name == "submit_review":
                    return True
    return False


async def _rescue_submit_review(
    *,
    provider: LLMProvider,
    provider_key: str,
    base_messages: list[Message],
    surface: ToolSurface,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    agent_context: AgentContext,
    turn: TurnContext,
    event_queue: EventQueue,
    budget_manager: Any,
    observability: Any,
    token_usage_repo: Any,
    validators: list[Any] | None,
) -> Any:
    """One-shot constrained rescue when Review exhausted rounds without
    ``submit_review``.  Reuses the same conversation history; surface
    narrowed to ``[submit_review]`` only; appends a directive nudging
    the LLM to pick one of four clear decisions.  Best-effort: failures
    return ``None`` and the caller falls through to the existing
    ``parse_review_from_messages`` fallback."""
    submit_review_tool: Any = None
    for tool in getattr(surface, "_tools", []):
        if getattr(tool, "name", "") == "submit_review":
            submit_review_tool = tool
            break
    if submit_review_tool is None:
        logger.warning("review_rescue_no_submit_review_tool")
        return None
    rescue_surface = ToolSurface.for_review(
        registry, role_mcp_tools, decision_tools=[submit_review_tool]
    )
    rescue_messages = list(base_messages) + [
        Message(role="user", content=_REVIEW_RESCUE_DIRECTIVE)
    ]
    logger.info(
        "review_rescue_invoked",
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
            max_rounds=2,
            event_queue=event_queue,
            budget_manager=budget_manager,
            observability=observability,
            token_usage_repo=token_usage_repo,
            validators=validators,
            provider_key=provider_key,
            a2a_context=turn.a2a_context,
            terminate_after=["submit_review"],
            # Forced tool call -- make the LLM call a tool this round. The
            # rescue surface only carries ``submit_review`` so
            # "required" collapses to "you must call submit_review".
            tool_choice="required",
            turn_id=turn.turn_id,
            iteration=turn.iteration,
            trigger=describe_trigger(turn.trigger_event),
        )
    except Exception:
        logger.exception("review_rescue_failed")
        return None
    if not _submit_review_was_called(rescue_loop.messages):
        logger.warning(
            "review_rescue_did_not_submit",
            agent=turn.agent.handle,
            turn_id=turn.turn_id,
        )
    return rescue_loop


def parse_review_from_messages(messages: list[Message]) -> ReviewOutcome:
    for msg in reversed(messages):
        if msg.role == "assistant" and msg.tool_calls:
            for call in msg.tool_calls:
                if call.name == "submit_review":
                    try:
                        return ReviewOutcome.model_validate(call.arguments)
                    except Exception:
                        logger.warning("review_parse_failed", args=call.arguments)
    # Fallback: look for a JSON-shaped assistant message.
    for msg in reversed(messages):
        if msg.role == "assistant" and msg.content:
            try:
                data = json.loads(msg.content)
                return ReviewOutcome.model_validate(data)
            except Exception:
                break
    return ReviewOutcome(decision="done", notes="(no explicit review)")


async def run_review_phase(
    *,
    turn: TurnContext,
    plan: ExecutionPlan,
    execute_result: ExecuteResult,
    provider: LLMProvider,
    provider_key: str,
    registry: ToolRegistry,
    role_mcp_tools: list[Any],
    event_queue: EventQueue,
    agent_context: AgentContext,
    max_rounds: int = 3,
    budget_manager: Any = None,
    observability: Any = None,
    token_usage_repo: Any = None,
    validators: list[Any] | None = None,
    prompt_skill_registry: Any = None,
) -> ReviewOutcome:
    """Run the Review phase and return the resulting :class:`ReviewOutcome`."""
    trigger = describe_trigger(turn.trigger_event)
    await publish_phase_started(
        event_queue=event_queue,
        agent=turn.agent,
        turn_id=turn.turn_id,
        iteration=turn.iteration,
        phase="review",
        trigger=trigger,
    )

    meta = _build_review_meta_tools()
    surface = ToolSurface.for_review(
        registry, role_mcp_tools, decision_tools=list(meta)
    )
    system_prompt = build_review_prompt(
        turn.agent.definition,
        plan_summary=plan.summary(),
        execute_summary=(execute_result.text or "(empty)")[:2000],
        execute_tool_log=_format_execute_tool_log(execute_result.tool_executions),
        plan_tool_log=_format_plan_tool_log(turn.plan_tool_executions),
        earlier_iterations=render_iteration_ledger(
            turn.iteration_history, skip_names=PLAN_META_TOOL_NAMES
        ),
        skill_registry=prompt_skill_registry,
    )
    user = "Judge whether this turn is done, or what should happen next."
    if execute_result.missing_tools:
        user += (
            f"\n\nExecute tried to call tools not in its surface: "
            f"{execute_result.missing_tools}. "
            "Consider self_iterate to let Plan add them."
        )
    if execute_result.exhausted_rounds:
        user += "\n\nExecute exhausted its tool-round cap."
    messages = [
        Message(role="system", content=system_prompt),
        Message(role="user", content=user),
    ]
    loop = await run_tool_loop(
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
        terminate_after=["submit_review"],
        turn_id=turn.turn_id,
        iteration=turn.iteration,
        trigger=trigger,
    )
    turn.input_tokens += loop.input_tokens
    turn.output_tokens += loop.output_tokens
    turn.model_keys["review"] = loop.model or provider_key

    review_messages = loop.messages
    rescue_fired = False
    rescue_loop = None
    if not _submit_review_was_called(loop.messages):
        rescue_fired = True
        rescue_loop = await _rescue_submit_review(
            provider=provider,
            provider_key=provider_key,
            base_messages=loop.messages,
            surface=surface,
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
            review_messages = rescue_loop.messages
            turn.input_tokens += rescue_loop.input_tokens
            turn.output_tokens += rescue_loop.output_tokens

    outcome = parse_review_from_messages(review_messages)
    logger.info(
        "review_phase_complete",
        decision=outcome.decision,
        has_notes=bool(outcome.notes),
    )
    await publish_phase_completed(
        event_queue=event_queue,
        agent=turn.agent,
        turn_id=turn.turn_id,
        conversation_key=turn.stored_conversation_key,
        iteration=turn.iteration,
        phase="review",
        provider_key=provider_key,
        loop=loop,
        decision=outcome.decision,
        notes=outcome.notes[:500],
        tools_available=list(surface.names),
        rescue_fired=rescue_fired,
        rescue_loop=rescue_loop,
        trigger=trigger,
    )
    return outcome


__all__ = [
    "ReviewDecision",
    "ReviewOutcome",
    "parse_review_from_messages",
    "run_review_phase",
]
