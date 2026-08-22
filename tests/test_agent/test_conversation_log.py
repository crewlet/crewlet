"""Tests for the cross-turn conversation ledger.

The within-turn ledger's doctrine, one scope wider: elide payloads
never structure, writes are never dropped, reads stay marked so the next
turn re-runs them instead of trusting a remembered value.
"""

from __future__ import annotations

import json

from crewlet.agent.conversation_log import (
    SESSION_REASONING_LIMIT,
    SESSION_TRIGGER_LIMIT,
    SessionEntry,
    build_session_entry,
    render_conversation_history,
)
from crewlet.agent.iteration_log import (
    LEDGER_ARTIFACT_LIMIT,
    LEDGER_PLAN_SUMMARY_LIMIT,
    LEDGER_VALUE_LIMIT,
)


def _entry(**kw) -> SessionEntry:
    base = {
        "turn_id": "abcdef01-2345-6789-abcd-ef0123456789",
        "at": "2026-08-20T09:30",
        "trigger": "Dana: can you take another look?",
        "plan_summary": "1. Re-read the ticket\n2. Reply",
        "reply": "Took another look — the failure is in the retry path.",
        "decision": "done",
    }
    base.update(kw)
    return SessionEntry(**base)


# ── building an entry ────────────────────────────────────────────────


def test_writes_are_recorded_and_reads_are_marked():
    """The rule the whole ledger turns on. A write must never be
    dropped; a read renders with a marker so the next turn knows it may
    re-run it rather than reuse a stale value."""
    entry = build_session_entry(
        turn_id="t1",
        at="2026-08-20T09:30",
        trigger="Dana: ping",
        plan_summary="do the thing",
        plan_reasoning="because",
        tool_executions=[
            {"name": "jira_get_issue", "arguments": '{"key": "POC-7"}', "success": True},
            {"name": "jira_add_comment", "arguments": '{"key": "POC-7"}', "success": True},
        ],
        read_only_names=("jira_get_issue",),
        skip_names=(),
        reply="done",
        decision="done",
    )

    assert "jira_get_issue" in entry.tool_calls
    assert "(read)" in entry.tool_calls
    # The write is present and is NOT marked as a re-runnable read.
    assert "jira_add_comment" in entry.tool_calls
    write_line = [
        ln for ln in entry.tool_calls.splitlines() if "jira_add_comment" in ln
    ][0]
    assert "(read)" not in write_line


def test_meta_tools_are_skipped():
    """Plan scaffolding is never a delivery, so in a ledger whose job is
    "what already happened that matters" it is pure noise."""
    entry = build_session_entry(
        turn_id="t1",
        at="",
        trigger="",
        plan_summary="",
        plan_reasoning="",
        tool_executions=[{"name": "submit_plan", "arguments": "{}", "success": True}],
        read_only_names=(),
        skip_names={"submit_plan"},
        reply="",
        decision="done",
    )
    assert entry.tool_calls == "(none)"


def test_budgets_are_applied_once_at_write_time():
    """Elision happens where the engine still has the real arguments, so
    a reader never needs the raw execution dicts."""
    entry = build_session_entry(
        turn_id="t1",
        at="",
        trigger="D" * (SESSION_TRIGGER_LIMIT * 2),
        plan_summary="P" * (LEDGER_PLAN_SUMMARY_LIMIT * 2),
        plan_reasoning="R" * (SESSION_REASONING_LIMIT * 2),
        tool_executions=[
            {
                "name": "slack_post",
                "arguments": json.dumps({"text": "T" * (LEDGER_VALUE_LIMIT * 3)}),
                "success": True,
            }
        ],
        read_only_names=(),
        skip_names=(),
        reply="Y" * (LEDGER_ARTIFACT_LIMIT * 2),
        decision="done",
    )

    assert len(entry.trigger) <= SESSION_TRIGGER_LIMIT + 1
    assert len(entry.plan_summary) <= LEDGER_PLAN_SUMMARY_LIMIT + 1
    assert len(entry.plan_reasoning) <= SESSION_REASONING_LIMIT + 1
    assert len(entry.reply) <= LEDGER_ARTIFACT_LIMIT + 1
    # The payload was elided but the tool NAME — the structure — survives.
    assert "slack_post" in entry.tool_calls


# ── round-tripping ───────────────────────────────────────────────────


def test_an_entry_round_trips_through_json():
    entry = _entry()
    assert SessionEntry.from_dict(entry.to_dict()) == entry


def test_a_row_from_an_older_engine_decodes_to_less_not_to_an_error():
    """The IterationRecord stance. Losing a field costs the next turn
    some history; raising would cost it the whole turn."""
    decoded = SessionEntry.from_dict({"reply": "only this survived"})
    assert decoded.reply == "only this survived"
    assert decoded.plan_summary == ""


def test_a_garbage_row_decodes_to_an_empty_entry():
    assert SessionEntry.from_dict({}) == SessionEntry()


# ── rendering ────────────────────────────────────────────────────────


def test_no_entries_render_nothing():
    """The first turn of every conversation. The caller drops the whole
    section rather than emitting an empty heading."""
    assert render_conversation_history([]) == ""


def test_rendering_names_what_the_seat_did():
    out = render_conversation_history([_entry()])
    assert "Triggered by: Dana: can you take another look?" in out
    assert "You planned:" in out
    assert "You replied:" in out
    assert "2026-08-20T09:30" in out


def test_the_turn_id_is_abbreviated_not_dumped():
    out = render_conversation_history([_entry()])
    assert "turn abcdef01" in out
    assert "abcdef01-2345-6789-abcd-ef0123456789" not in out


def test_a_done_turn_does_not_announce_its_decision():
    """"done" is the unremarkable case; saying so on every entry is
    noise. A failed or iterated turn is worth flagging."""
    assert "Turn ended:" not in render_conversation_history([_entry()])
    assert "Turn ended: failed" in render_conversation_history(
        [_entry(decision="failed")]
    )


def test_entries_render_oldest_first():
    out = render_conversation_history(
        [_entry(at="2026-08-20T09:00"), _entry(at="2026-08-20T11:00")]
    )
    assert out.index("09:00") < out.index("11:00")


def test_max_entries_keeps_the_newest():
    out = render_conversation_history(
        [_entry(reply=str(i)) for i in range(5)], max_entries=2
    )
    assert "You replied: 3" in out
    assert "You replied: 4" in out
    assert "You replied: 0" not in out


def test_the_char_budget_drops_from_the_oldest_end():
    """Recency is what a follow-up needs: the turn before the newest
    message is the one most likely to have already answered it."""
    entries = [_entry(reply="R" * 400, at=f"2026-08-2{i}T09:00") for i in range(4)]
    out = render_conversation_history(entries, max_chars=900)

    assert len(out) <= 900
    assert "2026-08-23T09:00" in out  # newest kept
    assert "2026-08-20T09:00" not in out  # oldest dropped


def test_the_char_budget_never_renders_nothing():
    """One over-budget entry still renders: an empty block would tell
    the next turn this conversation is new, which is worse than a long
    one."""
    out = render_conversation_history([_entry(reply="R" * 5000)], max_chars=100)
    assert out != ""


def test_an_empty_field_renders_no_line():
    """A ledger of blank labels reads as though the turn did nothing
    rather than as though the field was not recorded."""
    out = render_conversation_history([_entry(trigger="", plan_summary="")])
    assert "Triggered by:" not in out
    assert "You planned:" not in out
    assert "You replied:" in out
