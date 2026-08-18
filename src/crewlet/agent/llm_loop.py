"""Shared LLM tool-call loop for every phase of the turn engine.

Every call into an LLM provider routes through this module: Plan,
Execute, Review, and ephemeral sub-agents. Doing so in one place
keeps the subtle budget / token-usage / observability / secret
redaction / validator logic from drifting across phases.

Public surface:

- :func:`run_tool_loop` — drives a single LLM call + tool-call loop
  against a ``ToolSurface``. Returns a :class:`LoopResult` with the
  final assistant text, accumulated token counts, executed tools, and
  whether the loop exited by completion or round-cap exhaustion.
- :func:`execute_tool` — dispatch a named tool against a
  ``ToolSurface`` (falling back to the tool registry for unknown
  names). Centralises validation and secret redaction.
- :func:`assistant_text_with_reasoning` — render a conversation's
  assistant turns as the one displayable string both the live
  ``AgentTurnProgress`` and the durable ``AgentPhaseCompleted`` carry
  as their ``response``, reasoning included.
- :func:`sanitize_tool_output` — strip control characters from tool
  output before it returns to the LLM (no length truncation).
- :func:`validate_tool_result` — reject binary / non-UTF-8 data,
  redact secrets, run custom validators.

Every phase of :class:`~crewlet.agent.turn.TurnEngine` -- Plan,
Execute, Review, and sub-agents -- routes through these functions;
they are the single implementation of the LLM + tool-call loop and
the only call site for budget/token/redaction logic.
"""

from __future__ import annotations

import asyncio
import json
import re
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from contextvars import ContextVar
from dataclasses import dataclass, field
from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.instance import AgentInstance
from crewlet.events.types import (
    AgentPhaseCompleted,
    AgentPhaseStarted,
    AgentTurnProgress,
    BudgetExhausted,
    Event,
    PromptSize,
    format_reasoning_and_content,
)
from crewlet.providers.llm.protocol import LLMProvider, Message
from crewlet.queue.protocol import EventQueue
from crewlet.tools.protocol import AgentContext, ToolResult, ToolResultValidator

logger = get_logger("agent.llm_loop")

# Patterns that indicate secrets/credentials in tool output.  Keeping
# the list here (rather than per-phase) ensures every phase uses the
# same redaction.
_SECRET_PATTERNS: list[tuple[re.Pattern[str], str]] = [
    (re.compile(r"sk-proj-[A-Za-z0-9_-]{20,}"), "[REDACTED:api-key]"),
    (re.compile(r"sk-[A-Za-z0-9_-]{20,}"), "[REDACTED:api-key]"),
    (re.compile(r"xoxb-[A-Za-z0-9-]{20,}"), "[REDACTED:slack-token]"),
    (re.compile(r"xoxp-[A-Za-z0-9-]{20,}"), "[REDACTED:slack-token]"),
    (re.compile(r"xoxs-[A-Za-z0-9-]{20,}"), "[REDACTED:slack-token]"),
    (re.compile(r"AKIA[A-Z0-9]{16}"), "[REDACTED:aws-key]"),
    (re.compile(r"ghp_[A-Za-z0-9]{36,}"), "[REDACTED:github-token]"),
    (re.compile(r"gho_[A-Za-z0-9]{36,}"), "[REDACTED:github-token]"),
    (re.compile(r"glpat-[A-Za-z0-9_-]{20,}"), "[REDACTED:gitlab-token]"),
    (re.compile(r"plane_api_[A-Za-z0-9_-]{20,}"), "[REDACTED:plane-token]"),
    (re.compile(r"plane_wh_[A-Za-z0-9_-]{20,}"), "[REDACTED:plane-webhook-secret]"),
    (
        re.compile(
            r"-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----"
            r"[\s\S]*?"
            r"-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----"
        ),
        "[REDACTED:private-key]",
    ),
    (re.compile(r"(?i)(?:password|passwd|pwd)\s*[:=]\s*\S+"), "[REDACTED:password]"),
]


# ---------------------------------------------------------------------------
# Tool-output sanitization + validation
# ---------------------------------------------------------------------------


def redact_secrets(text: str) -> str:
    """Replace known secret / credential patterns with redaction markers.

    The single redaction implementation, shared by tool-result validation
    and any other surface that ships free text to the dashboard / event
    store (e.g. a sandbox coding-agent transcript, which can echo a cloned
    repo URL's token or a printed key). Idempotent.
    """
    for pattern, replacement in _SECRET_PATTERNS:
        text = pattern.sub(replacement, text)
    return text


def sanitize_tool_output(output: str) -> str:
    """Sanitize tool output before it returns to the LLM.

    Strips control characters (keeping newlines and tabs). The full
    output is returned unmodified beyond that — tool results are never
    length-truncated, because truncation silently hides content the
    agent reasons over (e.g. the tail of a ``list_mcp_server_tools``
    listing, where the tool the agent needs may sort past any cap).
    Secret redaction and binary/UTF-8 rejection happen separately in
    :func:`validate_tool_result`.
    """
    return re.sub(r"[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]", "", output)


def validate_tool_result(
    result: ToolResult,
    *,
    validators: list[ToolResultValidator] | None = None,
) -> ToolResult:
    """Validate and sanitize a tool result.

    - Rejects non-UTF-8 / binary data.
    - Redacts secrets.
    - Runs custom ``validators`` (if any).
    """
    if not result.success:
        return result

    output = result.output

    try:
        output.encode("utf-8").decode("utf-8")
    except (UnicodeDecodeError, UnicodeEncodeError):
        logger.warning("tool_result_invalid_utf8")
        return ToolResult(success=False, error="Tool output contained invalid data")

    if "\x00" in output:
        logger.warning("tool_result_binary_data")
        return ToolResult(success=False, error="Tool output contained binary data")

    redacted_output = redact_secrets(output)
    if redacted_output != output:
        logger.warning("tool_result_secrets_redacted")
        output = redacted_output

    for validator in validators or []:
        try:
            output = validator.validate(output)
        except ValueError as exc:
            logger.warning(
                "tool_result_validator_rejected",
                validator_name=validator.name,
                error=str(exc),
            )
            return ToolResult(success=False, error=f"Validation failed: {exc}")

    # Preserve the suspend signal through validation (the loop reads it to
    # defer the tool call); validation only touches ``output``.
    return ToolResult(
        success=True,
        output=output,
        suspend=result.suspend,
        suspend_payload=result.suspend_payload,
    )


# ---------------------------------------------------------------------------
# Tool dispatch
# ---------------------------------------------------------------------------


async def execute_tool(
    name: str,
    arguments: dict[str, Any],
    context: AgentContext,
    *,
    surface: Any,
    validators: list[ToolResultValidator] | None = None,
) -> ToolResult:
    """Dispatch a tool call by name against ``surface``.

    ``surface`` is a ``ToolSurface``; callers wanting the full
    "registry + per-role MCP" lookup should pass a surface built from
    :meth:`ToolSurface.for_execute` with the full tool list.

    A call to a name not in the surface returns an unknown-tool error
    immediately -- the executor cannot see schemas it wasn't given.

    When the surface carries a required-skill guard
    (``surface.skill_guard`` -- see :mod:`crewlet.agent.skills.guard`),
    calls to tools covered by a ``required: true`` tool skill are
    rejected until the LLM has loaded that skill via
    ``load_tool_skill`` in the current session; successful loads are
    recorded on the guard here, at the same layer that enforces them.
    """
    logger.debug(
        "tool_executing",
        tool_name=name,
        args=arguments,
        agent_id=context.agent_id,
        phase=getattr(surface, "phase", ""),
    )
    tool = surface.lookup(name)
    if tool is None:
        phase = getattr(surface, "phase", "")
        logger.warning(
            "tool_unknown",
            tool_name=name,
            agent_id=context.agent_id,
            phase=phase,
        )
        return ToolResult(success=False, error=_unknown_tool_error(name, surface))
    guard = getattr(surface, "skill_guard", None)
    if guard is not None:
        blocked = await guard.check_tool(tool)
        if blocked is not None:
            return blocked
    result = await tool.execute(arguments, context)
    result = validate_tool_result(result, validators=validators)
    if guard is not None:
        # Deliberately the POST-validation success: a load_tool_skill
        # body a validator rejected never reached the LLM's context, so
        # recording it as loaded would satisfy the guard while the
        # practices are absent — the exact failure the guard exists to
        # prevent. The LLM must retry until the body actually lands.
        guard.observe(name, arguments, result.success)
    if result.success:
        logger.debug("tool_result", tool_name=name, success=True)
    else:
        # Errors from MCP discovery / activation / execute are the
        # single most useful signal for diagnosing "why did the agent
        # not respond?" in production -- INFO instead of DEBUG keeps
        # them visible in the default stdout stream without flooding it.
        logger.info(
            "tool_result",
            tool_name=name,
            success=False,
            error=result.error or "",
        )
    return result


def _unknown_tool_error(name: str, surface: Any) -> str:
    """Phase-aware error for an unknown-tool call.

    Plan and Execute both keep MCP tool schemas out of ``tools=[...]``
    by default so the per-phase LLM payload stays small; activation is
    opt-in via ``activate_tool(name)``.  A direct call to a catalogue
    name that hasn't been activated yet fails here with a message
    pointing the LLM at the activation path -- after activation the
    tool's schema lands in ``tools=[...]`` on the next round and the
    LLM can invoke it normally.
    """
    phase = getattr(surface, "phase", "")
    if phase in ("plan", "execute", "onboarding"):
        catalogue = getattr(surface, "catalogue_names", lambda: [])()
        has_activate = getattr(surface, "has", lambda _n: False)("activate_tool")
        # Grace / rescue surfaces are built without the discovery
        # meta-tools (``expose_catalogue=False`` + no ``meta_tools``).
        # Telling that LLM to call ``activate_tool`` /
        # ``list_mcp_server_tools`` would direct it at tools it doesn't
        # have. Fall back to the bare error in that case.
        if not has_activate:
            return f"Unknown tool: {name}"
        if name in catalogue:
            return (
                f"To use {name!r}, call `activate_tool(name={name!r})`.  "
                "That promotes the tool into your tools=[...] so you "
                "can call it on the next round.  No need to re-activate "
                "once active."
            )
        phase_label = phase.upper()
        return (
            f"Unknown tool: {name!r}. Not in the {phase_label}-phase "
            "catalogue -- check the `## Available tools` block in your "
            "system prompt, and use `list_mcp_server_tools(server=...)` "
            "to discover MCP tool names."
        )
    return f"Unknown tool: {name}"


# ---------------------------------------------------------------------------
# Budget consumption (shared across phases)
# ---------------------------------------------------------------------------


async def consume_budget(
    total: int,
    *,
    agent: AgentInstance,
    budget_manager: Any,
    event_queue: EventQueue,
) -> None:
    """Check and consume token budget. Raises ``RuntimeError`` on exhaustion.

    The spend names its own refusing scope: the check and the increment
    happen in one statement against the shared counter, so which budget
    said no comes back with the answer. Re-reading the caps afterwards to
    work it out — as this did — is a read a peer's spend can invalidate
    between the refusal and the report.
    """
    if budget_manager is None or total <= 0:
        return
    outcome = await budget_manager.spend(agent.id_str, total)
    if outcome.ok:
        return

    budget_type = outcome.rejected_scope or "org"
    used = outcome.rejected_used
    limit = outcome.rejected_limit

    logger.warning(
        "budget_exhausted",
        agent_id=agent.id_str,
        budget_type=budget_type,
        used=used,
        limit=limit,
    )
    event = BudgetExhausted(
        source=agent.role_name,
        agent_id=agent.id_str,
        role=agent.role_name,
        budget_type=budget_type,
        used_tokens=used,
        max_tokens=limit,
    )
    await event_queue.publish(f"crewlet.events.{event.type}", event)
    raise RuntimeError(
        f"Token budget exhausted for agent {agent.id_str} "
        f"({budget_type} budget: {used}/{limit})"
    )


# ---------------------------------------------------------------------------
# Tool-call loop
# ---------------------------------------------------------------------------


@dataclass
class LoopResult:
    """Outcome of a single :func:`run_tool_loop` invocation."""

    text: str
    input_tokens: int = 0
    output_tokens: int = 0
    tool_executions: list[dict[str, Any]] = field(default_factory=list)
    rounds_used: int = 0
    exhausted_rounds: bool = False
    model: str = ""
    messages: list[Message] = field(default_factory=list)

    # Suspend/resume: set when a tool returned
    # ``suspend=True`` in an ``allow_suspend`` loop. The loop left
    # ``pending_tool_call_id`` unanswered and ``messages`` is the partial
    # conversation; the engine persists it, ends the turn, and resumes the
    # loop later by appending that call's real result.
    suspended: bool = False
    pending_tool_call_id: str = ""
    pending_tool_name: str = ""
    suspend_payload: dict[str, Any] | None = None


@dataclass
class PhaseProgress:
    """Mutable, in-flight view of a running tool loop.

    :func:`run_tool_loop` refreshes this after every round.  Its whole
    reason to exist is the failure path: when the loop raises there is no
    :class:`LoopResult` to publish, so a phase that died used to leave
    behind only its ``agent_phase_started`` event -- the dashboard showed
    an in-flight call with no response and no reason (the "it just says
    no response" report).  The caller keeps a handle on this object, so
    its ``except`` block can still publish what the phase managed: the
    conversation so far, the tool calls that ran, the tokens already
    billed, and which round it was on when it died.

    ``messages`` is the caller's own list -- ``run_tool_loop`` mutates it
    in place -- so it is current even for a failure raised mid-round,
    before the loop refreshed anything else.
    """

    messages: list[Message] = field(default_factory=list)
    tool_executions: list[dict[str, Any]] = field(default_factory=list)
    input_tokens: int = 0
    output_tokens: int = 0
    rounds_used: int = 0
    model: str = ""
    text: str = ""

    def to_result(self) -> LoopResult:
        """Freeze the partial state into a :class:`LoopResult`.

        ``exhausted_rounds`` stays False: the loop did not run out of
        rounds, it died.  ``failed`` on the event carries that.
        """
        return LoopResult(
            text=self.text,
            input_tokens=self.input_tokens,
            output_tokens=self.output_tokens,
            tool_executions=list(self.tool_executions),
            rounds_used=self.rounds_used,
            exhausted_rounds=False,
            model=self.model,
            messages=list(self.messages),
        )


# The failure view of the phase currently on this task's stack.
# ``phase_failure_guard`` installs it; ``run_tool_loop`` reports into it
# without every intermediate caller having to thread a parameter through.
# A nested phase (a sub-agent spawned inside Execute) installs its own and
# shadows the host's for its duration, so each phase records its own
# failure; a rescue or extension loop is a *continuation* of its host
# phase and deliberately keeps reporting into the host's.
_ACTIVE_PHASE_PROGRESS: ContextVar[PhaseProgress | None] = ContextVar(
    "crewlet_active_phase_progress", default=None
)


ProgressCallback = Callable[[AgentTurnProgress], Awaitable[None]]

# Max corrective re-prompts when ``tool_choice="required"`` but the model
# returns prose with no tool call. Bounded so a model that can never
# emit the call doesn't burn an unbounded slice of the round budget; the
# caller's ``max_rounds`` is the harder bound (rescue/judge use 2).
_MAX_FORCED_TOOL_RETRIES = 2


def assistant_text_with_reasoning(messages: list[Message]) -> str:
    """Render a conversation's assistant turns as ONE displayable string.

    Each assistant message contributes its reasoning wrapped in
    ``<think>...</think>`` immediately before that round's visible
    content (see
    :func:`~crewlet.events.types.format_reasoning_and_content`, the
    shared wire-format rule), rounds joined by a blank line.

    This is the single builder of the ``response`` field on BOTH the
    per-phase :class:`~crewlet.events.types.AgentPhaseCompleted` record
    and the per-round :class:`~crewlet.events.types.AgentTurnProgress`
    live update.  One function over one message list is what makes the
    dashboard's live row and the finished turn you expand afterwards
    the same text: the live update used to be assembled from a separate
    content-only accumulator, so a reasoning model streamed its tool
    calls against an empty response and its thinking appeared only once
    the phase ended.
    """
    parts = [
        format_reasoning_and_content(msg.reasoning_content, msg.content)
        for msg in messages
        if msg.role == "assistant"
    ]
    return "\n\n".join(part for part in parts if part)


def _is_known_read(name: str, surface: Any) -> bool:
    """Whether ``name`` is annotated read-only on this surface.

    Deliberately narrow: only an explicit ``read_only: true`` counts.
    ``writes_to_shared_surface`` answers the opposite question and
    returns ``False`` for an all-unknown annotation set, which is right
    for its own callers and exactly wrong here — using it would exempt
    every unannotated tool from the fence.
    """
    from crewlet.tools.capabilities import resolve_annotations

    tool = surface.lookup(name)
    if tool is None:
        return False
    registry = getattr(surface, "_registry", None)
    lookup = getattr(registry, "annotations_for", None)
    return resolve_annotations(tool, lookup).read_only is True


async def run_tool_loop(
    *,
    provider: LLMProvider,
    messages: list[Message],
    surface: Any,
    context: AgentContext,
    agent: AgentInstance,
    max_rounds: int,
    event_queue: EventQueue,
    budget_manager: Any = None,
    observability: Any = None,
    token_usage_repo: Any = None,
    validators: list[ToolResultValidator] | None = None,
    on_progress: ProgressCallback | None = None,
    a2a_context: dict[str, Any] | None = None,
    provider_key: str = "",
    terminate_after: list[str] | None = None,
    tool_choice: str | None = None,
    turn_id: str = "",
    iteration: int = 0,
    trigger: dict[str, Any] | None = None,
    allow_suspend: bool = False,
    phase_progress: PhaseProgress | None = None,
) -> LoopResult:
    """Drive the LLM + tool-call loop for one phase.

    Mutates ``messages`` in place by appending assistant and tool
    messages. Stops when the LLM returns no tool calls, or the
    ``max_rounds`` cap is hit.

    ``provider_key`` is the name used when ``provider.model`` is
    unset -- e.g. ``"plan"`` when the caller wants the trace event to
    record which phase ran.

    ``turn_id`` / ``iteration`` stamp the per-round
    :class:`AgentTurnProgress` events with the same turn coordinates
    the phase events carry, so live consumers (the dashboard's agent
    page) can place in-flight rounds inside the right turn/phase
    grouping.  The phase itself comes from ``surface.phase`` (falling
    back to ``provider_key``), matching the ``prompt.size`` event.

    ``phase_progress`` is a :class:`PhaseProgress` the loop refreshes
    after every round, so a caller can report partial work should the
    loop raise.  It defaults to whichever
    :func:`phase_failure_guard` is active on this task, which is how
    every phase gets one without threading a parameter through.

    ``allow_suspend`` lets a tool suspend the loop: when a tool returns
    ``ToolResult(suspend=True)`` the loop leaves that call unanswered and
    returns ``LoopResult(suspended=True, ...)`` carrying the partial
    ``messages`` for the engine to persist + resume later (the detached
    sandbox tool). Only the Execute phase passes
    ``allow_suspend=True``; Plan / Review never persist a partial
    conversation, so a suspend result there is logged and ignored.
    """
    turn_input_tokens = 0
    turn_output_tokens = 0
    executions: list[dict[str, Any]] = []
    accumulated = ""
    model = ""
    initial_prompt = ""
    prompt_messages: list[dict[str, str]] = []
    if messages:
        prompt_messages = [
            {"role": m.role, "content": m.content} for m in messages if m.content
        ]
        if prompt_messages:
            initial_prompt = prompt_messages[0].get("content", "")[:200]

    phase_name = getattr(surface, "phase", "") or provider_key

    # Everything this loop appends to ``messages`` starts here.  A
    # resumed Execute phase is handed the whole pre-suspend conversation,
    # and its published phase record deliberately shows only the
    # post-resume slice (``response_messages``) so the earlier segment
    # isn't replayed as a second row.  Anchoring the live response to the
    # same slice is what keeps the streaming row and the record it turns
    # into identical on a resumed phase too.
    response_slice_start = len(messages)

    async def publish_progress(round_index: int) -> None:
        """Push the round's state to live consumers (the dashboard).

        Called once before the first provider call (``round_index=-1``,
        carrying the prompt), then twice per round: once the moment the
        model has spoken — so its reasoning and prose reach the live view
        before the round's tools run, not after the slowest one returns —
        and again once that round's tool results are in.  The projection
        folds each into the same in-flight call (same ``turn_id`` /
        ``phase`` / ``iteration`` / ``round_num``), so the row fills in
        rather than flickering.

        The response is rebuilt from the message slice on every publish
        rather than accumulated incrementally.  That is O(rounds) per
        publish and so quadratic over a phase — deliberately: one builder
        over one message list is what guarantees the live row and the
        phase record cannot disagree, and an accumulator would be a
        second assembly of the same field, which is exactly the drift
        this function exists to remove.  Measured, the trade is not
        close: a 30-round phase spends 0.5 ms total rebuilding, and even
        120 rounds of 4 KB responses costs 57 ms across a phase that made
        120 LLM calls.  The same publish serializes a 20 KB+ prompt.

        Failures are logged and swallowed.  This is a live view of the
        phase, never part of it: a broker hiccup on a progress publish
        must not kill a turn that is otherwise fine, and must never
        stand in for the real error when the phase is already dying.
        Every other telemetry publish in this module has always been
        guarded this way; this one reached further than the rest once it
        started firing before the first provider call, where an
        unguarded raise would end a phase that had not yet run.
        """
        progress = AgentTurnProgress(
            source=agent.role_name,
            agent_id=agent.id_str,
            role=agent.role_name,
            turn_id=turn_id,
            phase=phase_name,
            iteration=iteration,
            model=model,
            trigger=trigger or {},
            prompt=initial_prompt,
            prompt_messages=prompt_messages,
            response=assistant_text_with_reasoning(messages[response_slice_start:]),
            input_tokens=turn_input_tokens,
            output_tokens=turn_output_tokens,
            total_tokens=turn_input_tokens + turn_output_tokens,
            round_num=round_index,
            tool_executions=list(executions),
            a2a_context=a2a_context,
        )
        try:
            if on_progress is not None:
                await on_progress(progress)
            else:
                await event_queue.publish(
                    f"crewlet.events.{progress.type}",
                    progress,
                )
        except asyncio.CancelledError:
            raise
        except Exception:
            logger.exception(
                "turn_progress_publish_failed",
                agent_id=agent.id_str,
                phase=phase_name,
                round=round_index,
            )

    # Point the caller's failure view at the live conversation before the
    # first provider call, so a phase that dies on round one still reports
    # the prompt it died on rather than an empty record.
    if phase_progress is None:
        phase_progress = _ACTIVE_PHASE_PROGRESS.get()
    if phase_progress is not None:
        phase_progress.messages = messages

    # Emit a prompt.size trace event for the initial messages of this
    # phase so the dashboard can track slimming progress over time.
    try:
        system_chars = sum(len(m.content or "") for m in messages if m.role == "system")
        user_chars = sum(len(m.content or "") for m in messages if m.role == "user")
        approx_tokens = (system_chars + user_chars) // 4
        event = PromptSize(
            source=agent.role_name,
            agent_id=agent.id_str,
            role=agent.role_name,
            phase=phase_name,
            approximate_tokens=approx_tokens,
            system_chars=system_chars,
            user_chars=user_chars,
        )
        await event_queue.publish(f"crewlet.events.{event.type}", event)
    except Exception:
        logger.exception("prompt_size_publish_failed")

    # Put the phase's prompt on the live view BEFORE the first provider
    # call.  ``AgentPhaseStarted`` cannot carry it — every phase runner
    # publishes that event and only then builds its prompt — so the
    # projection seeds its placeholder call with no messages at all, and
    # the dashboard rendered "No prompt recorded" for the whole of the
    # first (and largest, and usually slowest) call of the phase.  The
    # onboarding pass showed it worst: one 60k-token prompt against a
    # reasoning model, and the operator could not see what the agent had
    # been asked until the round came back.
    #
    # ``round_num=-1`` is the "no round has answered yet" sentinel the
    # projection's own placeholder already uses, so this update reads as
    # zero rounds rather than claiming one.
    if prompt_messages:
        await publish_progress(-1)

    rounds_used = 0
    exhausted = False
    terminate_names = set(terminate_after or [])
    # Suspend/resume state. Only an ``allow_suspend``
    # loop (Execute) can suspend; Plan / Review never persist a partial
    # conversation, so a suspend result there is logged and ignored.
    suspended = False
    pending_tool_call_id = ""
    pending_tool_name = ""
    suspend_payload: dict[str, Any] | None = None
    # Forced-tool-call enforcement. When the caller passed
    # ``tool_choice="required"`` (the Plan/Review rescue, the extension judge)
    # a round that returns prose with NO tool call must not be accepted as a
    # clean finish: some endpoints ignore ``tool_choice`` and some models
    # "think then stop" without emitting the call, which silently defeats the
    # forced round. We re-prompt with an explicit corrective and retry within
    # the round budget. Counter so a pathological model can't burn the whole
    # budget on reprompts beyond a small cap.
    forced_retries = 0
    fence = getattr(context, "seat_fence", None)
    for round_num in range(max_rounds):
        rounds_used = round_num + 1
        # Seat fence, at the cheapest possible point: no tokens have been
        # spent this round and nothing has fired. A node whose lease
        # moved stops HERE rather than running the rest of the turn
        # beside the seat's new owner. Raises SeatLost.
        if fence is not None:
            fence()
        # Refresh tool defs at the top of every round so any in-loop
        # surface mutation is visible to the provider on this call,
        # not the one after.
        tool_defs = surface.to_tool_defs()
        logger.debug(
            "tool_round_starting",
            round=rounds_used,
            max_rounds=max_rounds,
            agent_id=agent.id_str,
            phase=getattr(surface, "phase", ""),
            tool_count=len(tool_defs),
        )
        # Default ``tool_choice``: when the surface has tools we let
        # the LLM decide; callers that need to force a call (e.g. the
        # Plan / Review rescue paths) pass
        # ``tool_choice="required"`` to make the LLM call SOME tool
        # this round. The Anthropic adapter maps "required" →
        # ``{"type": "any"}`` so the surface's sole tool is the only
        # possible choice.
        effective_tool_choice = (
            tool_choice if tool_choice is not None else "auto" if tool_defs else None
        )
        completion = await provider.complete(
            messages=messages,
            tools=tool_defs if tool_defs else None,
            tool_choice=effective_tool_choice,
        )
        if not model:
            model = getattr(provider, "model", "") or provider_key

        total = completion.tokens_used or (
            completion.input_tokens + completion.output_tokens
        )
        if completion.input_tokens or completion.output_tokens:
            effective_input = completion.input_tokens
            effective_output = completion.output_tokens
        else:
            effective_input = total
            effective_output = 0
        turn_input_tokens += effective_input
        turn_output_tokens += effective_output

        if total > 0:
            agent.input_tokens += effective_input
            agent.output_tokens += effective_output
            await consume_budget(
                total,
                agent=agent,
                budget_manager=budget_manager,
                event_queue=event_queue,
            )
            if observability is not None:
                observability.track_tokens(
                    agent_id=agent.id_str,
                    input_tokens=completion.input_tokens,
                    output_tokens=completion.output_tokens,
                    total_tokens=total,
                )
            if token_usage_repo is not None and agent.handle:
                try:
                    await token_usage_repo.increment(agent.handle, total)
                except Exception:
                    logger.exception(
                        "token_usage_persist_failed",
                        agent_id=agent.id_str,
                        handle=agent.handle,
                        tokens=total,
                    )

        if completion.content:
            if accumulated:
                accumulated += "\n\n"
            accumulated += completion.content

        # Keep the assistant message in the trace regardless of what we do
        # next, so downstream fallback parsers (parse_plan_from_messages,
        # parse_review_from_messages) and debuggers see what the model said.
        # Appended BEFORE the branch below so the live progress event and the
        # phase record are built from the same conversation — the message list
        # is what both read.
        messages.append(
            Message(
                role="assistant",
                content=completion.content,
                reasoning_content=completion.reasoning_content,
                thinking_blocks=completion.thinking_blocks,
                tool_calls=completion.tool_calls,
            )
        )

        if phase_progress is not None:
            phase_progress.input_tokens = turn_input_tokens
            phase_progress.output_tokens = turn_output_tokens
            phase_progress.rounds_used = rounds_used
            phase_progress.model = model
            phase_progress.text = accumulated
            phase_progress.tool_executions = executions

        # First checkpoint of the round: the model has spoken.  Publishing
        # here rather than only after the round's tools is what puts the
        # model's reasoning on the live view at the moment it exists —
        # including on the final, no-tool-call round, which ends the loop
        # below and so never reached the post-tool publish at all.  Rounds
        # that produced neither prose nor reasoning have nothing new to
        # show, so they skip it and cost no extra traffic.
        if completion.content or completion.reasoning_content:
            await publish_progress(round_num)

        if not completion.tool_calls:
            # Forced round: the caller demanded a tool call but the model
            # returned prose. Re-prompt and retry within budget instead of
            # accepting it as terminal — this is what makes the plan rescue and
            # the extension judge actually force their structured-output call.
            if (
                tool_choice == "required"
                and forced_retries < _MAX_FORCED_TOOL_RETRIES
                and round_num < max_rounds - 1
            ):
                forced_retries += 1
                want = (
                    " or ".join(f"`{n}`" for n in sorted(terminate_names))
                    if terminate_names
                    else "the required tool"
                )
                messages.append(
                    Message(
                        role="user",
                        content=(
                            "Your last response made no tool call. You MUST "
                            f"respond by calling {want} now — return only that "
                            "tool call, not prose or reasoning."
                        ),
                    )
                )
                logger.warning(
                    "forced_tool_call_retry",
                    round=rounds_used,
                    attempt=forced_retries,
                    agent_id=agent.id_str,
                    phase=getattr(surface, "phase", ""),
                )
                continue
            exhausted = False
            break

        terminated_by_tool = False
        for tool_call in completion.tool_calls:
            # The finest-grained fence, and the last point before an
            # external side effect. Skipped for tools KNOWN to be
            # read-only: refusing a read buys nothing (it produces no
            # duplicate effect) and costs the turn. Unknown is fenced —
            # most builtins carry no annotations, and treating "not
            # classified" as "safe" would leave the majority of the
            # outbound surface unfenced.
            if fence is not None and not _is_known_read(tool_call.name, surface):
                fence()
            tool_result = await execute_tool(
                tool_call.name,
                tool_call.arguments,
                context,
                surface=surface,
                validators=validators,
            )
            # Suspend: the tool kicked off detached work whose result lands
            # later (the sandbox tool). Leave THIS call unanswered — the
            # engine appends the real result on resume — and defer only it;
            # sibling calls in the same assistant turn are still resolved
            # inline so the persisted conversation has exactly one dangling
            # tool_use. Only the first suspend is honored (one detached job
            # per turn); the conversation must end with a single hole.
            if allow_suspend and tool_result.suspend and not suspended:
                suspended = True
                pending_tool_call_id = tool_call.id
                pending_tool_name = tool_call.name
                suspend_payload = tool_result.suspend_payload or {}
                executions.append(
                    {
                        "name": tool_call.name,
                        "arguments": json.dumps(tool_call.arguments),
                        "result": "(detached work launched; awaiting result)",
                        "success": True,
                    }
                )
                continue
            if tool_result.suspend and not allow_suspend:
                logger.warning(
                    "tool_suspend_ignored_outside_execute",
                    tool=tool_call.name,
                    phase=phase_name,
                )
            if tool_result.success:
                content = tool_result.output
            else:
                content = (
                    tool_result.error or tool_result.output or "Tool execution failed"
                )
            content = sanitize_tool_output(content)
            messages.append(
                Message(
                    role="tool",
                    content=content,
                    tool_call_id=tool_call.id,
                    name=tool_call.name,
                )
            )
            executions.append(
                {
                    "name": tool_call.name,
                    "arguments": json.dumps(tool_call.arguments),
                    "result": content,
                    "success": tool_result.success,
                }
            )
            if tool_call.name in terminate_names and tool_result.success:
                terminated_by_tool = True

        if phase_progress is not None:
            phase_progress.tool_executions = executions
            phase_progress.text = accumulated

        # Second checkpoint: this round's tool results are in.
        await publish_progress(round_num)
        if terminated_by_tool or suspended:
            exhausted = False
            break
    else:
        exhausted = True
        logger.debug(
            "max_tool_rounds_exhausted",
            max_rounds=max_rounds,
            agent_id=agent.id_str,
            phase=getattr(surface, "phase", ""),
        )

    return LoopResult(
        text=accumulated,
        input_tokens=turn_input_tokens,
        output_tokens=turn_output_tokens,
        tool_executions=executions,
        rounds_used=rounds_used,
        exhausted_rounds=exhausted,
        model=model,
        messages=messages,
        suspended=suspended,
        pending_tool_call_id=pending_tool_call_id,
        pending_tool_name=pending_tool_name,
        suspend_payload=suspend_payload,
    )


# Phase-completed telemetry ships the full system / user / response
# text to the dashboard and event store -- no length caps.  Operators
# must be able to audit exactly what each phase saw and produced; a
# truncated prompt is what hid the tool an agent needed in the first
# place (see ``sanitize_tool_output``).


def _first_system_message(messages: list[Message]) -> str:
    for msg in messages:
        if msg.role == "system":
            return msg.content or ""
    return ""


def _first_user_message(messages: list[Message]) -> str:
    for msg in messages:
        if msg.role == "user":
            return msg.content or ""
    return ""


# Cap on the failure message carried on a phase event.  A provider
# traceback string or an MCP server's HTML error page can run to
# kilobytes; the dashboard renders the message inline in a failed-phase
# card, and the structured log keeps the untruncated original.  1 KiB
# holds every real provider / tool error message seen in practice with
# room to spare.
_ERROR_TEXT_LIMIT = 1024


def describe_failure(exc: BaseException) -> tuple[str, str]:
    """Return ``(message, kind)`` for a phase failure.

    ``kind`` is the machine-readable class the dashboard groups on.  An
    exhausted provider chain already carries the classified LLM error
    (``rate_limit`` / ``auth`` / ``timeout`` / ...), which is far more
    useful than the wrapper's type name, so it wins; a bare provider
    exception is classified the same way; anything else falls back to
    the exception's type name.
    """
    from crewlet.providers.errors import ProviderCallError, classify
    from crewlet.providers.fallback import LLMChainExhausted

    if isinstance(exc, LLMChainExhausted):
        kind = exc.last_error_kind or classify(exc.last_exc).value
        return f"{exc}", kind or type(exc).__name__
    if isinstance(exc, ProviderCallError):
        return str(exc), exc.kind.value
    if isinstance(exc, TimeoutError):
        return str(exc) or "timed out", "timeout"
    return str(exc) or type(exc).__name__, type(exc).__name__


@asynccontextmanager
async def phase_failure_guard(
    *,
    event_queue: EventQueue,
    agent: AgentInstance,
    turn_id: str,
    iteration: int,
    phase: str,
    provider_key: str = "",
    trigger: dict[str, Any] | None = None,
    host_phase: str = "",
    host_iteration: int = 0,
    notes: str = "",
) -> AsyncIterator[PhaseProgress]:
    """Publish a failed :class:`AgentPhaseCompleted` if the phase raises.

    Wrap a phase body in this and pass the yielded
    :class:`PhaseProgress` to :func:`run_tool_loop`.  On the happy path
    it does nothing -- the runner publishes its own completed event as
    usual.  When the body raises, it publishes the phase event the
    runner never reached, carrying the partial conversation plus the
    failure, and then **re-raises the original exception untouched** so
    every upstream handler (the turn engine's ``LLMChainExhausted`` ->
    ``LLMUnavailable`` path, its wall-clock ``TimeoutError`` classifier,
    the generic guard-breach path) behaves exactly as before.

    Without this, a phase that died published nothing at all: the last
    durable trace of the turn was the ``agent_phase_started`` that opened
    it, and the dashboard rendered an in-flight LLM call whose response
    never arrived -- "No response text yet" where the error should be.

    ``CancelledError`` is deliberately *not* recorded: a cancelled phase
    is the engine shutting the turn down, not the phase failing, and
    publishing during cancellation would fight the drain.
    """
    progress = PhaseProgress()
    token = _ACTIVE_PHASE_PROGRESS.set(progress)
    try:
        yield progress
    except asyncio.CancelledError:
        raise
    except Exception as exc:
        message, kind = describe_failure(exc)
        logger.warning(
            "phase_failed",
            phase=phase,
            agent_id=agent.id_str,
            turn_id=turn_id,
            iteration=iteration,
            error_kind=kind,
            error=message,
            rounds_used=progress.rounds_used,
        )
        try:
            await publish_phase_completed(
                event_queue=event_queue,
                agent=agent,
                turn_id=turn_id,
                iteration=iteration,
                phase=phase,
                provider_key=provider_key,
                loop=progress.to_result(),
                notes=notes,
                host_phase=host_phase,
                host_iteration=host_iteration,
                trigger=trigger,
                tag_span=False,
                failed=True,
                error=message,
                error_kind=kind,
            )
        except Exception:
            # Telemetry must never replace the real failure.
            logger.exception("phase_failed_publish_failed", phase=phase)
        raise
    finally:
        _ACTIVE_PHASE_PROGRESS.reset(token)


async def publish_phase_started(
    *,
    event_queue: EventQueue,
    agent: AgentInstance,
    turn_id: str,
    iteration: int,
    phase: str,
    trigger: dict[str, Any] | None = None,
) -> None:
    """Emit :class:`AgentPhaseStarted` for live-state dashboards.

    Called from each main phase runner (``run_plan_phase``,
    ``run_execute_phase``, ``run_review_phase``) right before the LLM
    loop opens.  Pair with :func:`publish_phase_completed` so the
    agent-state projection can flip ``current_phase`` on start and
    keep it across the (gap-free) handoff to the next phase, clearing
    only on ``TaskCompleted`` / ``TaskFailed``.

    Publish failures are logged but never abort a turn -- telemetry
    must not break the LLM loop.
    """
    event = AgentPhaseStarted(
        source=agent.role_name,
        agent_id=agent.id_str,
        role=agent.role_name,
        turn_id=turn_id,
        iteration=iteration,
        phase=phase,
        trigger=trigger or {},
    )
    try:
        await event_queue.publish(f"crewlet.events.{event.type}", event)
    except Exception:
        logger.exception("phase_started_publish_failed", phase=phase)


async def publish_phase_completed(
    *,
    event_queue: EventQueue,
    agent: AgentInstance,
    turn_id: str,
    iteration: int,
    phase: str,
    provider_key: str,
    loop: LoopResult,
    decision: str = "",
    notes: str = "",
    tools_available: list[str] | None = None,
    tool_catalogue: list[str] | None = None,
    rescue_fired: bool = False,
    rescue_loop: LoopResult | None = None,
    host_phase: str = "",
    host_iteration: int = 0,
    trigger: dict[str, Any] | None = None,
    tag_span: bool = True,
    response_messages: list[Message] | None = None,
    failed: bool = False,
    error: str = "",
    error_kind: str = "",
) -> None:
    """Emit :class:`AgentPhaseCompleted` and tag the current OTel span.

    Called from each phase runner (``run_plan_phase``, ``run_execute_phase``,
    ``run_review_phase``, ``spawn_subagent``) right after the LLM loop
    returns.  Fans detail out to two sinks:

    1. **Event**: full (truncated) system prompt, user prompt, response,
       tool executions, tokens, decision -- one row per phase per turn
       iteration.  Dashboards that query the events table by ``turn_id``
       can now render an accurate per-phase timeline without having to
       reconstruct it from ``AgentTurnProgress`` round-events.

    2. **Span attributes**: lightweight gen_ai.* attributes on the
       currently-open phase span (created by ``TurnEngine._child_phase``
       or ``spawn_subagent``) so OTel trace viewers show per-phase
       token counts, model, and decision at a glance.  Full text lives
       on the event; spans only carry identity + counts.

    ``rescue_loop``: when the phase's main loop exhausted its
    rounds without producing the structured artifact, the runner fires
    a constrained rescue / grace call (``_rescue_submit_plan`` /
    ``_rescue_submit_review`` / ``_grace_summarize_execute``). That
    second loop is a *continuation* of the same phase -- so its tool
    executions, tokens, and rounds are merged into this one event, and
    the ``response`` is taken from the rescue (its ``submit_*`` call /
    wrap-up is the real artifact). Without the merge the event would
    show only the exhausted main loop and the rescue's work -- the
    actual ``submit_plan`` call, its tokens -- would be invisible in
    the per-phase telemetry while still counted at the turn level.

    ``failed`` / ``error`` / ``error_kind``: set by
    :func:`phase_failure_guard` when the phase raised.  The event is then
    a record of a *dead* phase -- ``loop`` carries whatever
    :class:`PhaseProgress` captured before the exception, which is
    partial by construction.

    Errors in either sink are logged but do not propagate -- telemetry
    must never abort a turn.
    """
    system_prompt = _first_system_message(loop.messages)
    user_prompt = _first_user_message(loop.messages)
    # Include the model's reasoning content as ``<think>...</think>``
    # blocks so the dashboard's existing think-block renderer shows
    # extended-thinking output.  Falls back to plain ``loop.text``
    # when no reasoning was emitted.  When a rescue loop ran, its
    # messages carry the real artifact (the ``submit_*`` call / the
    # wrap-up summary) -- prefer them for the response.
    response_source = rescue_loop if rescue_loop is not None else loop
    # On a RESUMED phase the runner passes ``response_messages`` — only the
    # post-resume slice of the conversation — so the event shows just the
    # continuation, not a replay of the pre-suspend segment (which was already
    # published as its own checkpoint). A rescue loop carries
    # its own messages and ignores the slice.
    src_messages = (
        response_messages
        if (rescue_loop is None and response_messages is not None)
        else response_source.messages
    )
    response = assistant_text_with_reasoning(src_messages) or response_source.text

    # Merge main + rescue loop counters so the per-phase event reflects
    # everything the phase did. ``exhausted_rounds`` keeps the *main*
    # loop's signal: paired with ``rescue_fired`` it reads as "main
    # loop exhausted, rescue recovered it".
    tool_executions = list(loop.tool_executions)
    input_tokens = loop.input_tokens
    output_tokens = loop.output_tokens
    rounds_used = loop.rounds_used
    if rescue_loop is not None:
        tool_executions += list(rescue_loop.tool_executions)
        input_tokens += rescue_loop.input_tokens
        output_tokens += rescue_loop.output_tokens
        rounds_used += rescue_loop.rounds_used
    total_tokens = input_tokens + output_tokens
    model = loop.model or provider_key

    event = AgentPhaseCompleted(
        source=agent.role_name,
        agent_id=agent.id_str,
        role=agent.role_name,
        turn_id=turn_id,
        iteration=iteration,
        phase=phase,
        host_phase=host_phase,
        host_iteration=host_iteration,
        model=model,
        provider_key=provider_key,
        trigger=trigger or {},
        system_prompt=system_prompt,
        user_prompt=user_prompt,
        response=response,
        tool_executions=tool_executions,
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        total_tokens=total_tokens,
        rounds_used=rounds_used,
        exhausted_rounds=loop.exhausted_rounds,
        decision=decision,
        rescue_fired=rescue_fired,
        notes=notes,
        tools_available=list(tools_available or []),
        tool_catalogue=list(tool_catalogue or []),
        failed=failed,
        error=error[:_ERROR_TEXT_LIMIT],
        error_kind=error_kind,
    )
    try:
        await event_queue.publish(f"crewlet.events.{event.type}", event)
    except Exception:
        logger.exception("phase_completed_publish_failed", phase=phase)

    if tag_span:
        tag_phase_span(
            phase=phase,
            iteration=iteration,
            turn_id=turn_id,
            rounds_used=rounds_used,
            exhausted_rounds=loop.exhausted_rounds,
            rescue_fired=rescue_fired,
            tool_executions_count=len(tool_executions),
            decision=decision,
            model=model,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            tools_available=tools_available,
            tool_catalogue=tool_catalogue,
        )


def tag_phase_span(
    *,
    phase: str,
    iteration: int,
    turn_id: str,
    rounds_used: int,
    exhausted_rounds: bool,
    rescue_fired: bool,
    tool_executions_count: int,
    decision: str,
    model: str,
    input_tokens: int,
    output_tokens: int,
    tools_available: list[str] | None = None,
    tool_catalogue: list[str] | None = None,
) -> None:
    """Tag the currently-active OTel span with phase attributes.

    Separated from :func:`publish_phase_completed` so callers that
    DEFER event publication (the extension judge -- its event fires
    after the host phase event so chronological dashboards stay
    ordered) can tag their span eagerly while it's still the current
    span.  Without this split the deferred publish would call
    ``get_current_span()`` after its own span has closed and end up
    tagging the host phase's span instead, clobbering the host's
    attributes.

    Phase events that publish synchronously route through
    ``publish_phase_completed(tag_span=True)`` and never call this
    helper directly.
    """
    try:
        from opentelemetry import trace as _otel_trace

        span = _otel_trace.get_current_span()
        if span is None or not span.is_recording():
            return
        span.set_attribute("phase.name", phase)
        span.set_attribute("phase.iteration", iteration)
        span.set_attribute("phase.turn_id", turn_id)
        span.set_attribute("phase.rounds_used", rounds_used)
        span.set_attribute("phase.exhausted_rounds", exhausted_rounds)
        span.set_attribute("phase.rescue_fired", rescue_fired)
        span.set_attribute("phase.tool_executions_count", tool_executions_count)
        if decision:
            span.set_attribute("phase.decision", decision)
        if model:
            span.set_attribute("gen_ai.request.model", model)
        span.set_attribute("gen_ai.usage.input_tokens", input_tokens)
        span.set_attribute("gen_ai.usage.output_tokens", output_tokens)
        if tools_available:
            span.set_attribute("phase.tools_available_count", len(tools_available))
            # OTel recommends sending array attributes as tuples of
            # strings; comma-joined fallback keeps exporters that
            # stringify attributes (e.g. the console exporter)
            # readable.
            span.set_attribute("phase.tools_available", ",".join(tools_available[:50]))
        if tool_catalogue:
            span.set_attribute("phase.tool_catalogue_count", len(tool_catalogue))
            span.set_attribute("phase.tool_catalogue", ",".join(tool_catalogue[:50]))
    except Exception:
        logger.exception("phase_span_attr_failed", phase=phase)


__all__ = [
    "LoopResult",
    "Event",  # re-exported for convenience
    "consume_budget",
    "execute_tool",
    "PhaseProgress",
    "describe_failure",
    "phase_failure_guard",
    "publish_phase_completed",
    "publish_phase_started",
    "redact_secrets",
    "run_tool_loop",
    "sanitize_tool_output",
    "tag_phase_span",
    "validate_tool_result",
]
