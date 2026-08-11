"""Shared helpers for matching events to agents."""

from __future__ import annotations

from typing import Any

# Tag keys that identify an agent's involvement in an event.
# Used by both stores for ``related_agent`` filtering and by
# ``_row_to_event`` to reconstruct tags from payload.
AGENT_TAG_KEYS = ("agent_role", "target", "recipient", "sender")

# All tag keys the writer extracts (superset of AGENT_TAG_KEYS).
# Used by ``_row_to_event`` to reconstruct the full tags dict.
ALL_TAG_KEYS = (
    "agent_id",
    "agent_role",
    "task_id",
    "channel_id",
    "sender",
    "target",
    "recipient",
    "requester",
    "closed_by",
)


def event_relates_to_agent(event: dict[str, Any], name: str) -> bool:
    """Return True if *event* is directly related to the agent *name*.

    Matches when the agent name appears in:
    - ``actor`` — the agent produced the event
    - ``tags[agent_role|target|recipient|sender]`` — the event was
      routed to, sent to, or involves the agent
    """
    if event.get("actor") == name:
        return True
    tags = event.get("tags") or {}
    return any(tags.get(k) == name for k in AGENT_TAG_KEYS)


def collect_related_events(
    all_events: list[dict[str, Any]],
    name: str,
    limit: int,
) -> list[dict[str, Any]]:
    """Return events related to *name*, including trace context.

    First finds events that directly match the agent (via
    ``event_relates_to_agent``), then includes other events from the
    same traces — so inbound triggers (webhooks, notifications) that
    caused the agent's work are included in the results.
    """
    # Pass 1: find directly matching events and collect their trace_ids.
    direct: list[dict[str, Any]] = []
    trace_ids: set[str] = set()
    for e in all_events:
        if event_relates_to_agent(e, name):
            direct.append(e)
            tid = e.get("trace_id", "")
            if tid:
                trace_ids.add(tid)

    if not trace_ids:
        return direct[:limit]

    # Pass 2: add events from the same traces (dedup by id).
    seen = {e.get("id") for e in direct}
    result = list(direct)
    for e in all_events:
        eid = e.get("id")
        if eid in seen:
            continue
        if e.get("trace_id", "") in trace_ids:
            result.append(e)
            seen.add(eid)

    # Sort newest first and cap.
    result.sort(key=lambda e: e.get("timestamp", ""), reverse=True)
    return result[:limit]
