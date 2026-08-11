"""Token-spend aggregation for the dashboard's Tokens view.

Reads ``agent_phase_completed`` events via
:meth:`EventStore.list_phase_token_events` and rolls them up by phase /
model / worker / agent / turn so the dashboard can render breakdowns in
a single fetch.

Kept separate from :mod:`crewlet.api.routes` so the aggregation is
unit-testable in isolation against any iterable of phase-event dicts —
no Starlette request plumbing required.
"""

from __future__ import annotations

from collections import defaultdict
from collections.abc import Iterable
from typing import Any


def _empty_bucket() -> dict[str, int]:
    return {
        "input_tokens": 0,
        "output_tokens": 0,
        "total_tokens": 0,
        "calls": 0,
    }


def _accumulate(bucket: dict[str, int], event: dict[str, Any]) -> None:
    bucket["input_tokens"] += int(event.get("input_tokens", 0) or 0)
    bucket["output_tokens"] += int(event.get("output_tokens", 0) or 0)
    bucket["total_tokens"] += int(event.get("total_tokens", 0) or 0)
    bucket["calls"] += 1


def _sorted_by_tokens(
    rows: Iterable[dict[str, Any]],
) -> list[dict[str, Any]]:
    return sorted(rows, key=lambda r: r.get("total_tokens", 0), reverse=True)


def aggregate_phase_events(
    events: list[dict[str, Any]],
    *,
    role_handle_map: dict[str, str] | None = None,
    recent_turns_limit: int = 50,
) -> dict[str, Any]:
    """Roll up per-phase events into the breakdown shape.

    ``role_handle_map`` lets the agent rollup include each role's
    handle (used by the dashboard to link a per-agent row back to its
    agent detail page).  When omitted, ``handle`` is left blank.

    ``recent_turns_limit`` caps the per-turn list; the table is meant
    to show a tail of recent activity, not the full history.
    """
    handles = role_handle_map or {}

    totals = _empty_bucket()
    by_phase: dict[str, dict[str, int]] = defaultdict(_empty_bucket)
    by_model: dict[str, dict[str, int]] = defaultdict(_empty_bucket)
    by_worker: dict[str, dict[str, int]] = defaultdict(_empty_bucket)
    by_agent: dict[str, dict[str, Any]] = {}
    by_turn: dict[str, dict[str, Any]] = {}

    # High-water mark: the latest event timestamp this rollup aggregated.
    # The dashboard folds live ``agent_phase_completed`` events onto this
    # baseline (see static/dashboard/js/store.js) and uses the watermark
    # to skip events the baseline already counted — without it, an event
    # that is both in this baseline and re-delivered on the live stream
    # would be double-counted.  ISO-8601 strings sort lexically by time.
    aggregated_through = ""

    for event in events:
        phase = event.get("phase", "") or "unknown"
        model = event.get("model", "") or "unknown"
        worker = event.get("worker", "")
        role = event.get("agent_role", "") or "unknown"
        turn_id = event.get("turn_id", "")
        timestamp = event.get("timestamp", "")
        agent_id = event.get("agent_id", "")

        if timestamp and timestamp > aggregated_through:
            aggregated_through = timestamp

        _accumulate(totals, event)
        _accumulate(by_phase[phase], event)
        _accumulate(by_model[model], event)
        if phase == "auxiliary" and worker:
            _accumulate(by_worker[worker], event)

        # Per-agent rollup with nested per-phase split.
        agent_bucket = by_agent.setdefault(
            role,
            {
                "role": role,
                "handle": handles.get(role, ""),
                "agent_id": agent_id,
                "input_tokens": 0,
                "output_tokens": 0,
                "total_tokens": 0,
                "calls": 0,
                "by_phase": defaultdict(_empty_bucket),
            },
        )
        # Keep the latest agent_id we observe so the dashboard can
        # cross-link even when the runtime id changes across sessions.
        if agent_id:
            agent_bucket["agent_id"] = agent_id
        _accumulate(agent_bucket, event)
        _accumulate(agent_bucket["by_phase"][phase], event)

        # Per-turn rollup with nested per-phase split and the turn's
        # earliest timestamp so dashboards can sort newest-first.
        if turn_id:
            turn_bucket = by_turn.setdefault(
                turn_id,
                {
                    "turn_id": turn_id,
                    "role": role,
                    "handle": handles.get(role, ""),
                    "agent_id": agent_id,
                    "started_at": timestamp,
                    "ended_at": timestamp,
                    "input_tokens": 0,
                    "output_tokens": 0,
                    "total_tokens": 0,
                    "calls": 0,
                    "by_phase": defaultdict(_empty_bucket),
                },
            )
            if timestamp:
                # ``started_at`` = earliest, ``ended_at`` = latest.  ISO
                # 8601 strings sort lexically by time, so plain min/max
                # gives the right ordering.
                started = turn_bucket["started_at"]
                ended = turn_bucket["ended_at"]
                if not started or timestamp < started:
                    turn_bucket["started_at"] = timestamp
                if not ended or timestamp > ended:
                    turn_bucket["ended_at"] = timestamp
            _accumulate(turn_bucket, event)
            _accumulate(turn_bucket["by_phase"][phase], event)

    # Project defaultdicts → plain dicts and order each rollup so the
    # consumer doesn't need to sort.
    by_phase_list = _sorted_by_tokens(
        {"phase": phase, **bucket} for phase, bucket in by_phase.items()
    )
    by_model_list = _sorted_by_tokens(
        {"model": model, **bucket} for model, bucket in by_model.items()
    )
    by_worker_list = _sorted_by_tokens(
        {"worker": worker, **bucket} for worker, bucket in by_worker.items()
    )

    by_agent_list = _sorted_by_tokens(
        {**bucket, "by_phase": dict(bucket["by_phase"])} for bucket in by_agent.values()
    )

    by_turn_list = sorted(
        (
            {**bucket, "by_phase": dict(bucket["by_phase"])}
            for bucket in by_turn.values()
        ),
        key=lambda r: r.get("ended_at", ""),
        reverse=True,
    )[:recent_turns_limit]

    return {
        "totals": totals,
        "by_phase": by_phase_list,
        "by_model": by_model_list,
        "by_worker": by_worker_list,
        "by_agent": by_agent_list,
        "by_turn": by_turn_list,
        "aggregated_through": aggregated_through,
    }
