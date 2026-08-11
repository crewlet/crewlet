"""Tests for the shared agent-matching helpers used by ``list_events``.

These cover ``event_relates_to_agent`` and ``collect_related_events``
— the logic that powers the ``related_agent`` filter in both the
memory and persistent stores.
"""

from __future__ import annotations

from typing import Any

from crewlet.timescaledb._match import (
    AGENT_TAG_KEYS,
    ALL_TAG_KEYS,
    collect_related_events,
    event_relates_to_agent,
)


def _event(
    id: str,  # noqa: A002 — matches the field name used by the stores
    *,
    actor: str = "",
    tags: dict[str, str] | None = None,
    trace_id: str = "",
    timestamp: str = "",
) -> dict[str, Any]:
    return {
        "id": id,
        "actor": actor,
        "tags": tags or {},
        "trace_id": trace_id,
        "timestamp": timestamp,
    }


# --------------------------------------------------------------------------- #
# Constants
# --------------------------------------------------------------------------- #


def test_agent_tag_keys_is_subset_of_all_tag_keys() -> None:
    assert set(AGENT_TAG_KEYS).issubset(set(ALL_TAG_KEYS))


# --------------------------------------------------------------------------- #
# event_relates_to_agent
# --------------------------------------------------------------------------- #


def test_event_relates_via_actor() -> None:
    assert event_relates_to_agent(_event("e1", actor="CTO"), "CTO")


def test_event_relates_via_agent_role_tag() -> None:
    assert event_relates_to_agent(_event("e1", tags={"agent_role": "PM"}), "PM")


def test_event_relates_via_target_tag() -> None:
    assert event_relates_to_agent(_event("e1", tags={"target": "Dev"}), "Dev")


def test_event_relates_via_recipient_tag() -> None:
    assert event_relates_to_agent(_event("e1", tags={"recipient": "QA"}), "QA")


def test_event_relates_via_sender_tag() -> None:
    assert event_relates_to_agent(_event("e1", tags={"sender": "Ops"}), "Ops")


def test_event_does_not_relate_to_unrelated_agent() -> None:
    ev = _event("e1", actor="CTO", tags={"agent_role": "CTO"})
    assert not event_relates_to_agent(ev, "PM")


def test_event_relates_handles_missing_tags() -> None:
    """Events without a ``tags`` key must still match by ``actor``."""
    ev_no_tags = {"id": "e1", "actor": "PM"}
    assert event_relates_to_agent(ev_no_tags, "PM")
    assert not event_relates_to_agent(ev_no_tags, "CTO")


def test_event_relates_handles_none_tags() -> None:
    """``tags`` explicitly set to ``None`` must not crash the helper."""
    ev = {"id": "e1", "actor": "PM", "tags": None}
    assert event_relates_to_agent(ev, "PM")
    assert not event_relates_to_agent(ev, "CTO")


def test_event_relates_ignores_agent_id_tag() -> None:
    """``agent_id`` is a runtime UUID, not an agent name — not a match key."""
    # event_relates_to_agent should only match names, so an agent_id
    # equal to the agent name should NOT count (and neither should the
    # agent name accidentally appearing in agent_id).
    ev = _event("e1", tags={"agent_id": "PM"})
    assert not event_relates_to_agent(ev, "PM")


# --------------------------------------------------------------------------- #
# collect_related_events
# --------------------------------------------------------------------------- #


def test_collect_returns_only_direct_matches_when_no_trace() -> None:
    events = [
        _event("e1", actor="CTO"),
        _event("e2", actor="PM"),
        _event("e3", tags={"agent_role": "CTO"}),
    ]
    result = collect_related_events(events, "CTO", limit=10)
    ids = {e["id"] for e in result}
    assert ids == {"e1", "e3"}


def test_collect_includes_trace_siblings_of_direct_matches() -> None:
    """Unrelated events from the same trace are still included."""
    events = [
        _event("webhook", tags={"target": "CTO"}, trace_id="tr-1"),
        _event("turn", actor="CTO", trace_id="tr-1"),
        _event("log", actor="system", trace_id="tr-1"),  # sibling, not direct
        _event("unrelated", actor="PM", trace_id="tr-2"),  # other trace
    ]
    result = collect_related_events(events, "CTO", limit=10)
    ids = {e["id"] for e in result}
    assert ids == {"webhook", "turn", "log"}
    assert "unrelated" not in ids


def test_collect_limits_direct_matches_when_no_trace_ids() -> None:
    events = [_event(f"e{i}", actor="CTO") for i in range(20)]
    result = collect_related_events(events, "CTO", limit=5)
    assert len(result) == 5


def test_collect_sorts_newest_first_when_trace_expansion_happens() -> None:
    events = [
        _event("old", actor="CTO", trace_id="tr-1", timestamp="2026-04-12T10:00:00"),
        _event("new", actor="CTO", trace_id="tr-1", timestamp="2026-04-12T12:00:00"),
        _event("mid", actor="other", trace_id="tr-1", timestamp="2026-04-12T11:00:00"),
    ]
    result = collect_related_events(events, "CTO", limit=10)
    # With trace_id present the result is sorted newest-first.
    timestamps = [e["timestamp"] for e in result]
    assert timestamps == sorted(timestamps, reverse=True)


def test_collect_deduplicates_by_id_when_event_is_both_direct_and_sibling() -> None:
    events = [
        _event("match", actor="CTO", trace_id="tr-1"),  # direct match
        _event("sibling", actor="PM", trace_id="tr-1"),
    ]
    result = collect_related_events(events, "CTO", limit=10)
    ids = [e["id"] for e in result]
    # ``match`` must appear exactly once even though it's both a direct
    # match and a sibling of itself.
    assert ids.count("match") == 1


def test_collect_returns_empty_when_no_matches() -> None:
    events = [_event("e1", actor="PM"), _event("e2", actor="Dev")]
    result = collect_related_events(events, "CTO", limit=10)
    assert result == []


def test_collect_handles_empty_trace_id_for_direct_matches() -> None:
    """Direct matches without a trace are returned as-is (no sibling expansion)."""
    events = [
        _event("a", actor="CTO", trace_id=""),
        _event("b", actor="CTO", trace_id=""),
    ]
    result = collect_related_events(events, "CTO", limit=10)
    assert {e["id"] for e in result} == {"a", "b"}
