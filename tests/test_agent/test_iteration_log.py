"""Tests for the per-turn iteration ledger."""

from __future__ import annotations

import json

from crewlet.agent.iteration_log import (
    LEDGER_ARTIFACT_LIMIT,
    LEDGER_BLOB_LIMIT,
    LEDGER_MAX_READ_CALLS,
    LEDGER_NOTE_LIMIT,
    LEDGER_PLAN_SUMMARY_LIMIT,
    LEDGER_VALUE_LIMIT,
    IterationRecord,
    format_tool_calls,
    render_iteration_ledger,
)

# ---------------------------------------------------------------------------
# format_tool_calls
# ---------------------------------------------------------------------------


def test_format_tool_calls_empty_returns_explicit_none():
    """An empty log must read as "no action taken", not vanish."""
    assert format_tool_calls([]) == "(none)"


def test_format_tool_calls_defaults_to_verbatim_arguments():
    """Review's single-iteration evidence log depends on full args:
    it is what delivery is judged against, so nothing may be elided."""
    body = "x" * 5000
    log = format_tool_calls(
        [
            {
                "name": "slack_post",
                "arguments": json.dumps({"text": body}),
                "success": True,
            }
        ]
    )
    assert body in log
    assert "…" not in log


def test_format_tool_calls_marks_failures_as_errors():
    """A failed delivery is not delivery — and the next round may retry
    it, so the outcome has to be visible."""
    log = format_tool_calls(
        [
            {
                "name": "slack_post",
                "arguments": "{}",
                "success": False,
                "result": "channel_not_found",
            }
        ]
    )
    assert "→ error: channel_not_found" in log


def test_format_tool_calls_flags_unknown_tool_results():
    """The loop returns the error string without setting ``success``."""
    log = format_tool_calls(
        [{"name": "nope", "arguments": "{}", "result": "Unknown tool: nope"}]
    )
    assert "→ error: Unknown tool: nope" in log


def test_format_tool_calls_skips_named_tools():
    log = format_tool_calls(
        [
            {"name": "submit_plan", "arguments": "{}", "success": True},
            {"name": "slack_post", "arguments": "{}", "success": True},
        ],
        skip_names={"submit_plan"},
    )
    assert "submit_plan" not in log
    assert "slack_post" in log


def test_format_tool_calls_all_skipped_returns_none():
    log = format_tool_calls(
        [{"name": "submit_plan", "arguments": "{}", "success": True}],
        skip_names={"submit_plan"},
    )
    assert log == "(none)"


# ---------------------------------------------------------------------------
# Elision: per-value, never per-blob
# ---------------------------------------------------------------------------


def test_elision_preserves_every_argument_key():
    """The regression this shape exists for.

    ``json.dumps`` preserves key order, so capping the serialised object
    would drop whichever keys sort last — and the discriminating
    argument (``channel``) is the SHORT one here. Losing it would leave a
    ledger line that looks precise while hiding WHICH delivery fired.
    """
    args = json.dumps({"text": "y" * 4000, "channel": "#eng-updates"})
    log = format_tool_calls(
        [{"name": "slack_post", "arguments": args, "success": True}],
        value_limit=LEDGER_VALUE_LIMIT,
        blob_limit=LEDGER_BLOB_LIMIT,
    )
    assert "channel" in log
    assert "#eng-updates" in log
    assert "text" in log
    assert "y" * 400 not in log


def test_elision_keeps_short_values_untouched():
    args = json.dumps({"key": "INFRA-412", "notify": True, "count": 3})
    log = format_tool_calls(
        [{"name": "jira_comment", "arguments": args, "success": True}],
        value_limit=LEDGER_VALUE_LIMIT,
        blob_limit=LEDGER_BLOB_LIMIT,
    )
    assert "INFRA-412" in log
    # Native JSON types survive rather than being stringified.
    assert "true" in log
    assert "3" in log


def test_elision_applies_blob_backstop_for_many_arguments():
    """Even fully elided values add up when a call takes ~40 of them."""
    args = json.dumps({f"field_{i}": f"value-{i}" for i in range(80)})
    log = format_tool_calls(
        [{"name": "wide_tool", "arguments": args, "success": True}],
        value_limit=LEDGER_VALUE_LIMIT,
        blob_limit=LEDGER_BLOB_LIMIT,
    )
    assert len(log) < LEDGER_BLOB_LIMIT + 100
    assert log.rstrip().endswith("→ success")


def test_elision_handles_non_json_arguments():
    """Anything that doesn't parse as a JSON object is opaque text."""
    log = format_tool_calls(
        [{"name": "odd", "arguments": "z" * 900, "success": True}],
        value_limit=LEDGER_VALUE_LIMIT,
        blob_limit=LEDGER_BLOB_LIMIT,
    )
    assert "…" in log
    # No keys to preserve, so the whole-argument budget applies directly.
    assert len(log) < LEDGER_BLOB_LIMIT + 100


def test_elision_truncates_long_error_text():
    log = format_tool_calls(
        [
            {
                "name": "slack_post",
                "arguments": "{}",
                "success": False,
                "result": "e" * 3000,
            }
        ],
        value_limit=LEDGER_VALUE_LIMIT,
        blob_limit=LEDGER_BLOB_LIMIT,
    )
    assert "→ error:" in log
    assert len(log) < LEDGER_VALUE_LIMIT + 200


# ---------------------------------------------------------------------------
# render_iteration_ledger
# ---------------------------------------------------------------------------


def test_render_ledger_empty_returns_empty_string():
    """First iteration of every turn: callers drop the whole section
    rather than emit a heading over nothing."""
    assert render_iteration_ledger([]) == ""


def test_render_ledger_includes_both_phase_logs_and_review_prose():
    rec = IterationRecord(
        iteration=1,
        plan_summary="Post the summary to the channel.",
        plan_tool_calls=(
            {"name": "jira_get", "arguments": '{"key": "INFRA-1"}', "success": True},
        ),
        execute_tool_calls=(
            {
                "name": "slack_post",
                "arguments": '{"channel": "#eng"}',
                "success": True,
            },
        ),
        execute_text="Q3 spend was $412k.",
        review_notes="Add the breakdown.",
        completed_work="The post already landed.",
    )
    out = render_iteration_ledger([rec])
    assert "### Iteration 1" in out
    assert "Planned: Post the summary to the channel." in out
    assert "Plan called:" in out
    assert "jira_get(" in out
    assert "Execute called:" in out
    assert "slack_post(" in out
    assert "Produced: Q3 spend was $412k." in out
    assert "Reviewer, on what already landed: The post already landed." in out
    assert "Reviewer's correction: Add the breakdown." in out


def test_render_ledger_filters_meta_tools_from_both_phases():
    """Meta-tools are never a delivery, so in a ledger whose only job is
    "what already happened that matters" they are pure noise."""
    rec = IterationRecord(
        iteration=1,
        plan_tool_calls=({"name": "submit_plan", "arguments": "{}", "success": True},),
        execute_tool_calls=(
            {"name": "load_tool_skill", "arguments": "{}", "success": True},
        ),
    )
    out = render_iteration_ledger([rec], skip_names={"submit_plan", "load_tool_skill"})
    assert "submit_plan" not in out
    assert "load_tool_skill" not in out
    assert out.count("(none)") == 2


def test_render_ledger_omits_reviewer_prose_when_absent():
    """The engine's ``done`` → ``self_iterate`` override records a
    ledger with no ``completed_work`` — the tool calls still carry."""
    rec = IterationRecord(
        iteration=1,
        execute_tool_calls=(
            {"name": "slack_post", "arguments": "{}", "success": True},
        ),
        review_notes="Execute produced an answer as text but did not deliver.",
    )
    out = render_iteration_ledger([rec])
    assert "Reviewer, on what already landed" not in out
    assert "slack_post(" in out
    assert "Reviewer's correction:" in out


def test_render_ledger_accumulates_iterations_in_order():
    out = render_iteration_ledger(
        [
            IterationRecord(iteration=1, review_notes="first"),
            IterationRecord(iteration=2, review_notes="second"),
        ]
    )
    assert out.index("### Iteration 1") < out.index("### Iteration 2")


def test_render_ledger_elides_long_artifacts_and_plan_summaries():
    rec = IterationRecord(
        iteration=1,
        plan_summary="p" * 9000,
        execute_text="a" * 9000,
    )
    out = render_iteration_ledger([rec])
    assert "…" in out
    # Both blocks are guarded, so a pathological 18k of LLM output still
    # lands within the two budgets plus headings.
    assert len(out) < LEDGER_PLAN_SUMMARY_LIMIT + LEDGER_ARTIFACT_LIMIT + 400


# ---------------------------------------------------------------------------
# Blob backstop must drop whole keys, never cut the serialised object
# ---------------------------------------------------------------------------


def test_blob_backstop_drops_keys_instead_of_cutting_the_object():
    """Regression: the backstop used to `_elide` the serialised JSON.

    With several bulky values, per-value elision alone still exceeds the
    blob budget, and cutting the string removed whichever keys sorted
    last — re-introducing the exact loss per-value elision exists to
    prevent, one step later. Identifiers must survive; payloads are what
    gets dropped.
    """
    args = json.dumps(
        {
            "text": "A" * 400,
            "blocks": "B" * 400,
            "attachments": "C" * 400,
            "metadata": "D" * 400,
            "unfurl": "E" * 400,
            "thread_ts": "1712345678.000200",
            "channel": "#eng-updates",
        }
    )
    log = format_tool_calls(
        [{"name": "slack_post", "arguments": args, "success": True}],
        value_limit=LEDGER_VALUE_LIMIT,
        blob_limit=LEDGER_BLOB_LIMIT,
    )
    # The two identifiers that say WHICH delivery fired must survive.
    assert "#eng-updates" in log
    assert "1712345678.000200" in log
    # And the line must admit what it dropped rather than look complete.
    assert "more" in log


def test_blob_backstop_keeps_valid_json_and_counts_the_remainder():
    args = json.dumps({f"f{i}": "v" * 200 for i in range(10)} | {"id": "X1"})
    log = format_tool_calls(
        [{"name": "wide", "arguments": args, "success": True}],
        value_limit=LEDGER_VALUE_LIMIT,
        blob_limit=LEDGER_BLOB_LIMIT,
    )
    assert '"id": "X1"' in log
    rendered = log[log.index("(") + 1 : log.rindex(") →")]
    payload, _, tail = rendered.partition(" +")
    json.loads(payload)  # must still parse — no mid-object cut
    assert tail.endswith("more")


def test_blob_backstop_keeps_one_key_when_nothing_fits():
    """A single oversized argument still names itself rather than
    rendering an empty object."""
    args = json.dumps({"body": "z" * 5000})
    log = format_tool_calls(
        [{"name": "big", "arguments": args, "success": True}],
        value_limit=LEDGER_VALUE_LIMIT,
        blob_limit=20,
    )
    assert "body" in log


# ---------------------------------------------------------------------------
# Reads are marked so the next round may re-run them
# ---------------------------------------------------------------------------


def test_reads_are_marked_and_writes_are_not():
    """Tool RESULTS are not carried across iterations, so a read the
    next round needs must be re-runnable. Marking is what lets the
    prompt permit that without also permitting a re-delivery."""
    log = format_tool_calls(
        [
            {"name": "jira_get_issue", "arguments": "{}", "success": True},
            {"name": "slack_post", "arguments": "{}", "success": True},
        ],
        read_only_names={"jira_get_issue"},
    )
    assert "jira_get_issue({}) → success (read)" in log
    assert "slack_post({}) → success" in log
    assert "slack_post({}) → success (read)" not in log


def test_read_cap_never_drops_writes():
    """Only reads are dropped to fit the per-phase line cap — a write
    is the entire reason the ledger exists."""
    calls = [
        {"name": "search", "arguments": f'{{"q": {i}}}', "success": True}
        for i in range(30)
    ]
    calls.append({"name": "slack_post", "arguments": "{}", "success": True})
    log = format_tool_calls(
        calls,
        read_only_names={"search"},
        max_read_calls=LEDGER_MAX_READ_CALLS,
    )
    assert log.count("- search(") == LEDGER_MAX_READ_CALLS
    assert "slack_post" in log
    assert "further read call(s) omitted" in log


def test_ledger_marks_reads_from_the_record():
    rec = IterationRecord(
        iteration=1,
        execute_tool_calls=(
            {"name": "jira_get_issue", "arguments": "{}", "success": True},
            {"name": "slack_post", "arguments": "{}", "success": True},
        ),
        read_only_names=("jira_get_issue",),
    )
    out = render_iteration_ledger([rec])
    assert "jira_get_issue({}) → success (read)" in out
    assert "slack_post({}) → success\n" in out or out.endswith(
        "slack_post({}) → success"
    )


# ---------------------------------------------------------------------------
# Reviewer prose is budgeted like every other rendered field
# ---------------------------------------------------------------------------


def test_reviewer_prose_is_elided_like_other_fields():
    rec = IterationRecord(
        iteration=1,
        review_notes="n" * 4000,
        completed_work="c" * 4000,
    )
    out = render_iteration_ledger([rec])
    assert "…" in out
    assert len(out) < 2 * LEDGER_NOTE_LIMIT + 400


# ---------------------------------------------------------------------------
# Round-trip across a sandbox suspend
# ---------------------------------------------------------------------------


def test_record_round_trips_through_dict():
    rec = IterationRecord(
        iteration=2,
        plan_summary="P",
        plan_tool_calls=({"name": "a", "arguments": "{}", "success": True},),
        execute_tool_calls=({"name": "b", "arguments": "{}", "success": True},),
        read_only_names=("a",),
        execute_text="T",
        review_notes="N",
        completed_work="C",
    )
    assert IterationRecord.from_dict(json.loads(json.dumps(rec.to_dict()))) == rec


def test_record_from_dict_tolerates_a_pre_ledger_row():
    """A sandbox run suspended by an older engine has no ledger key in
    its persisted state; it must resume, not raise."""
    assert IterationRecord.from_dict({}) == IterationRecord(iteration=0)
