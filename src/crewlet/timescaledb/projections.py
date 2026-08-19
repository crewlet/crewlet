"""Row → API-shape projections shared by every event-store backend.

The store backends (:mod:`crewlet.timescaledb.repository` over
PostgreSQL, :mod:`crewlet.timescaledb.memory` for tests and
store-less deployments) answer the same protocol, so they must project
rows into *byte-identical* shapes.  They used to do that by carrying
hand-maintained copies of the same field whitelist, which is exactly how
a field goes missing: when
:class:`~crewlet.events.types.AgentPhaseCompleted` gained its failure
fields, a copy that forgot to list them silently deleted the failure on
its way to the dashboard — the reason a failed LLM turn rendered as "No
response text yet" rather than the error.

Everything here takes a plain payload dict, so a projection is
unit-testable without a database and impossible to drift between
backends.
"""

from __future__ import annotations

from typing import Any

__all__ = [
    "llm_history_record",
    "phase_token_record",
]


def llm_history_record(
    event_type: str, timestamp: str, payload: dict[str, Any]
) -> dict[str, Any]:
    """Normalize a turn/phase event into the LLM-history record shape.

    Phase events (``agent_phase_completed``) carry ``system_prompt`` +
    ``user_prompt`` separately; turn events (``agent_turn_completed``)
    carry a single ``prompt`` string plus a ``prompt_messages`` list.
    Both flatten to one shape so the dashboard renders them uniformly.

    Both branches carry ``failed`` / ``error`` / ``error_kind``: a phase
    that died is a first-class record, not an absence.  The turn branch
    also reports its real ``decision`` — it used to hardcode ``""``,
    which blanked the single most useful bit about a failed turn
    (``decision == "failed"``) on its way out of the store.
    """
    failure = {
        "failed": bool(payload.get("failed", False)),
        "error": payload.get("error", "") or "",
        "error_kind": payload.get("error_kind", "") or "",
    }

    if event_type == "agent_phase_completed":
        system_prompt = payload.get("system_prompt", "")
        user_prompt = payload.get("user_prompt", "")
        messages: list[dict[str, str]] = []
        if system_prompt:
            messages.append({"role": "system", "content": system_prompt})
        if user_prompt:
            messages.append({"role": "user", "content": user_prompt})
        return {
            "timestamp": timestamp,
            "model": payload.get("model", "") or payload.get("provider_key", ""),
            "phase": payload.get("phase", ""),
            "host_phase": payload.get("host_phase", ""),
            "host_iteration": payload.get("host_iteration", 0),
            # Set on phase=="auxiliary" rows so the dashboard can show
            # which learning worker (persist_decider, skill_synthesizer,
            # counterparty_profiler, …) made the call.
            "worker": payload.get("worker", ""),
            "turn_id": payload.get("turn_id", ""),
            "iteration": payload.get("iteration", 0),
            "decision": payload.get("decision", ""),
            "notes": payload.get("notes", ""),
            # The event that triggered this turn — the dashboard renders
            # it as the LLM invocation's source.
            "trigger": payload.get("trigger", {}) or {},
            "prompt": user_prompt,
            "prompt_messages": messages,
            "response": payload.get("response", ""),
            "input_tokens": payload.get("input_tokens", 0),
            "output_tokens": payload.get("output_tokens", 0),
            "total_tokens": payload.get("total_tokens", 0),
            "rounds_used": payload.get("rounds_used", 0),
            "exhausted_rounds": bool(payload.get("exhausted_rounds", False)),
            "rescue_fired": bool(payload.get("rescue_fired", False)),
            "backend": payload.get("backend", "native"),
            "coding_agent": payload.get("coding_agent", ""),
            "tool_executions": payload.get("tool_executions", []),
            "tools_available": payload.get("tools_available", []),
            "tool_catalogue": payload.get("tool_catalogue", []),
            **failure,
        }
    return {
        "timestamp": timestamp,
        "model": payload.get("model", ""),
        "phase": "turn",
        "turn_id": payload.get("turn_id", ""),
        "iteration": 0,
        "decision": payload.get("decision", ""),
        "notes": "",
        "trigger": payload.get("trigger", {}) or {},
        "prompt": payload.get("prompt", ""),
        "prompt_messages": payload.get("prompt_messages", []),
        "response": payload.get("response", ""),
        "input_tokens": payload.get("input_tokens", 0),
        "output_tokens": payload.get("output_tokens", 0),
        "total_tokens": payload.get("total_tokens", 0),
        "rounds_used": 0,
        "exhausted_rounds": False,
        "rescue_fired": False,
        "backend": "native",
        "coding_agent": "",
        "tool_executions": payload.get("tool_executions", []),
        "tools_available": [],
        "tool_catalogue": [],
        **failure,
    }


def phase_token_record(
    *,
    event_id: str,
    timestamp: str,
    agent_id: str,
    agent_role: str,
    payload: dict[str, Any],
) -> dict[str, Any]:
    """Project an ``agent_phase_completed`` row to the Tokens-view shape.

    Kept minimal — only the fields the breakdown aggregation needs.
    Heavy payload fields stay out so a wide window doesn't transfer
    megabytes: the prompts and the response obviously, but also
    ``tool_executions``, whose ``result`` strings are never length-capped
    (a whole page, a sandbox transcript) and which no consumer of this
    record reads.  The live projection retains these records for its
    rollup window, so anything carried here is carried resident.
    """
    return {
        "event_id": event_id,
        "timestamp": timestamp,
        "agent_id": agent_id,
        "agent_role": agent_role,
        "phase": payload.get("phase", ""),
        "host_phase": payload.get("host_phase", ""),
        "worker": payload.get("worker", ""),
        "model": payload.get("model", "") or payload.get("provider_key", ""),
        "turn_id": payload.get("turn_id", ""),
        "iteration": int(payload.get("iteration", 0) or 0),
        "input_tokens": int(payload.get("input_tokens", 0) or 0),
        "output_tokens": int(payload.get("output_tokens", 0) or 0),
        "total_tokens": int(payload.get("total_tokens", 0) or 0),
        "failed": bool(payload.get("failed", False)),
    }
