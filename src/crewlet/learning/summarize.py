"""Cheap-model summarisation for episode-search hits.

Runs on the role's ``llm_auxiliary`` model (falls back to ``llm``) so
the Plan phase sees a compact rendering instead of the raw JSON
payload.  Falls back to the raw payload when no auxiliary model can
be resolved or when the provider call fails — summarisation is an
optimisation, never a blocker.
"""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.learning._aux_telemetry import complete_with_telemetry
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import LLMProvider, Message
from crewlet.telemetry import set_span_error, tracer

logger = get_logger("learning.summarize")


_SUMMARIZE_SYSTEM_PROMPT = """You are summarising a list of the
agent's past completed turns (episodes), surfaced to help the agent
plan a new, similar task.

The input contains two kinds of entries:

1. ``kind: raw`` -- a single past turn.  Emit one bullet:
   - [<review_outcome>] <what was done> -- tools: <n1, n2, ...>

2. ``kind: compacted`` -- an aggregate summary of N similar past
   turns that were compacted by the lifecycle worker.  Emit one
   bullet that signals the aggregate framing:
   - [pattern, observed <count>×] <common_task_pattern> -- typically:
     <common_outcome>, success_rate <success_rate>; subjects:
     <subjects_involved>; notes: <notable_patterns>

Keep each bullet tight (one line, no internal newlines).  No prose
before or after the bullet list.  Preserve input order.  Never
invent episodes; never merge raw entries into the compacted one or
vice versa."""


async def summarize_episodes(
    *,
    raw_payload: str,
    role: Role,
    llm_providers: dict[str, LLMProvider],
    max_tokens: int = 400,
    event_queue: Any = None,
    agent_id: str = "",
    turn_id: str = "",
) -> str:
    """Summarise ``raw_payload`` via the role's auxiliary model.

    Returns the model's bullet list on success, ``raw_payload``
    unchanged on any failure.  Two distinct fall-through paths:

    * ``resolve_phase_provider`` raises when ``llm_providers`` is
      empty / missing the configured key.  Logged as
      ``summarize_no_provider``; this is the in-memory / test mode
      where summarisation is simply unavailable.
    * The provider itself raises (network, auth, rate limit, etc.).
      Logged as ``summarize_llm_failed``.
    """
    if not raw_payload:
        return raw_payload
    with tracer.start_as_current_span(
        "learning.summarize_episodes",
        attributes={
            "learning.worker": "summarize_episodes",
            "learning.role": role.name,
            "learning.payload_chars": len(raw_payload),
            "learning.max_tokens": max_tokens,
        },
    ) as span:
        try:
            provider_key, provider = resolve_phase_provider(
                role, "auxiliary", llm_providers
            )
        except Exception:
            span.set_attribute("learning.outcome", "skipped")
            span.set_attribute("learning.skip_reason", "provider_unavailable")
            logger.debug("summarize_no_provider")
            return raw_payload
        span.set_attribute("learning.provider_key", provider_key)
        try:
            completion = await complete_with_telemetry(
                provider=provider,
                messages=[
                    Message(role="system", content=_SUMMARIZE_SYSTEM_PROMPT),
                    Message(role="user", content=raw_payload),
                ],
                worker="summarize_episodes",
                role_name=role.name,
                provider_key=provider_key,
                event_queue=event_queue,
                agent_id=agent_id,
                turn_id=turn_id,
                temperature=0.1,
                max_tokens=max_tokens,
            )
        except Exception as exc:
            span.set_attribute("learning.outcome", "failed")
            set_span_error(exc)
            logger.exception("summarize_llm_failed")
            return raw_payload
        text = (completion.content or "").strip()
        if not text:
            span.set_attribute("learning.outcome", "noop")
            return raw_payload
        span.set_attribute("learning.outcome", "done")
        span.set_attribute("learning.output_chars", len(text))
        logger.debug("episodes_summarized", length=len(text))
        return text
