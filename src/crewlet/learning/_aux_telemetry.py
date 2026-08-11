"""Telemetry wrapper for auxiliary LLM calls.

Each learning worker (PersistDecider, CounterpartyProfiler,
SkillSynthesizer, SkillRefiner, PromotionSynthesizer,
``summarize_episodes``) reaches an LLM via the role's
``llm_auxiliary`` provider.  Those calls are invisible to the
dashboard's per-agent "LLM Invocations" view today because that view
reads ``AgentPhaseCompleted`` events — which only fire from the main
turn loop's Plan / Execute / Review phases.

This helper wraps ``provider.complete`` so each aux call emits an
``AgentPhaseCompleted`` event with ``phase="auxiliary"`` and the
worker's name, populating the same ``system_prompt`` / ``user_prompt``
/ ``response`` / token-count fields the main phases use.  The
dashboard then renders the aux call alongside the main phases without
any frontend change.

Best-effort: a publish failure must not surface back to the worker
(reflection is "never breaks the engine" by design).
"""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import AgentPhaseCompleted
from crewlet.providers.llm.protocol import Completion, LLMProvider, Message

logger = get_logger("learning.aux_telemetry")

# Aux-LLM telemetry stamps the full system / user / response text on
# the event -- no length caps, so operators can audit exactly what each
# learning worker saw and produced.

# Hard deadline on a single aux-LLM call.  Every learning worker and
# every Plan-phase relevance prefetch reaches its model through this
# helper; without a deadline a hung provider stalls the whole
# Plan-phase prefetch ``gather`` (or a reflection worker) indefinitely.
# The callers all catch exceptions and degrade gracefully, so a
# :class:`TimeoutError` here surfaces as an empty prefetch block / a
# skipped worker rather than a wedged turn.
_AUX_CALL_TIMEOUT_SECONDS = 60.0


def format_response_with_reasoning(completion: Completion) -> str:
    """Combine reasoning + visible content into one string for the event.

    Mirrors :func:`crewlet.agent.llm_loop._assistant_text_with_reasoning`
    so aux-LLM calls render thinking the same way main-phase calls do
    in the dashboard: reasoning is wrapped in ``<think>...</think>``
    and prepended to the visible content.

    The dashboard's ``renderLLMResponse`` already understands those
    tags, so this is a zero-frontend-change fix.  Empty reasoning
    yields the visible content unchanged (preserving the
    pre-existing event shape for non-thinking models).

    Public so callers that pass a ``response_formatter`` to
    :func:`complete_with_telemetry` can build on top of it (e.g. the
    personal-memory filter appends a "→ Resolved selection:" block
    after this base format).
    """
    reasoning = (getattr(completion, "reasoning_content", "") or "").strip()
    content = (completion.content or "").strip()
    if not reasoning:
        return content
    if not content:
        # Cap-hit shape: the model produced reasoning but no visible
        # text.  Surfacing the thinking is the only signal the
        # operator has -- without this they see ``response=""`` and
        # have to read the raw structlog to know thinking happened.
        return f"<think>{reasoning}</think>"
    return f"<think>{reasoning}</think>\n\n{content}"


async def complete_with_telemetry(
    *,
    provider: LLMProvider,
    messages: list[Message],
    worker: str,
    role_name: str,
    provider_key: str,
    event_queue: Any = None,
    agent_id: str = "",
    turn_id: str = "",
    response_formatter: Callable[[Completion], str] | None = None,
    timeout: float = _AUX_CALL_TIMEOUT_SECONDS,
    **complete_kwargs: Any,
) -> Completion:
    """Run one auxiliary ``provider.complete`` call and publish a
    matching ``AgentPhaseCompleted`` event.

    The ``provider.complete`` call is bounded by ``timeout`` seconds
    (default :data:`_AUX_CALL_TIMEOUT_SECONDS`).  On timeout an
    :class:`TimeoutError` is raised -- callers already catch it and
    degrade (an empty prefetch block, a skipped reflection worker) so
    a hung provider never wedges the host turn.

    The event publishes after the LLM call returns (so the response
    fields are populated).  When the call raises, no event fires —
    the worker's existing ``logger.exception`` already captures the
    failure path; surfacing a half-populated event would mislead the
    dashboard.  Pass ``event_queue=None`` to bypass publishing
    (test mode, or when the engine wasn't wired with one).

    ``response_formatter`` is an optional callable that maps the
    raw :class:`Completion` to the string stamped on the event's
    ``response`` field.  Defaults to
    :func:`format_response_with_reasoning` (which wraps reasoning
    in ``<think>`` tags + appends visible content).  Workers that
    want to enrich the dashboard view -- e.g. the personal-memory
    filter appending a ``→ Resolved selection:`` block -- pass a
    custom formatter that calls the default and decorates its
    output.  The completion returned to the caller is unaffected by
    the formatter, so worker logic that parses the raw response
    keeps working unchanged.
    """
    completion = await asyncio.wait_for(
        provider.complete(messages=messages, **complete_kwargs),
        timeout=timeout,
    )
    if event_queue is None:
        return completion
    try:
        # First message is the system prompt by convention; the last
        # is whatever the worker stamped as the "user" turn.
        system_prompt = ""
        user_prompt = ""
        if messages:
            if messages[0].role == "system":
                system_prompt = messages[0].content
            for m in reversed(messages):
                if m.role == "user":
                    user_prompt = m.content
                    break
        formatter = response_formatter or format_response_with_reasoning
        try:
            response_text = formatter(completion)
        except Exception:
            # A worker-supplied formatter raising must not lose the
            # event entirely; fall back to the default so the
            # dashboard at least sees the LLM output.
            logger.exception("aux_response_formatter_failed", worker=worker)
            response_text = format_response_with_reasoning(completion)
        # Defensive stringification: ``AgentPhaseCompleted.turn_id`` /
        # ``.agent_id`` are typed ``str``, but callers occasionally pass
        # raw UUID objects (e.g. ``Episode.turn_id``).  Coerce here so
        # the telemetry path absorbs the upstream type drift instead of
        # losing the event to a Pydantic ValidationError.
        event = AgentPhaseCompleted(
            source=role_name,
            agent_id=str(agent_id) if agent_id else "",
            role=role_name,
            turn_id=str(turn_id) if turn_id else "",
            phase="auxiliary",
            worker=worker,
            model=getattr(provider, "model", "") or provider_key,
            provider_key=provider_key,
            system_prompt=system_prompt,
            user_prompt=user_prompt,
            response=response_text,
            input_tokens=completion.input_tokens,
            output_tokens=completion.output_tokens,
            total_tokens=completion.input_tokens + completion.output_tokens,
        )
        await event_queue.publish("crewlet.events.agent_phase_completed", event)
    except Exception:
        logger.exception("aux_phase_completed_publish_failed", worker=worker)
    return completion
