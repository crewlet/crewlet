"""Tests for tool-call log rendering."""

from __future__ import annotations

import json

from crewlet.agent.iteration_log import format_tool_calls

# Budgets a many-calls consumer would pass; the renderer itself is
# policy-free and defaults to verbatim.
VALUE_LIMIT = 120
BLOB_LIMIT = 400

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
        value_limit=VALUE_LIMIT,
        blob_limit=BLOB_LIMIT,
    )
    assert "channel" in log
    assert "#eng-updates" in log
    assert "text" in log
    assert "y" * 200 not in log


def test_elision_keeps_short_values_untouched():
    args = json.dumps({"key": "INFRA-412", "notify": True, "count": 3})
    log = format_tool_calls(
        [{"name": "jira_comment", "arguments": args, "success": True}],
        value_limit=VALUE_LIMIT,
        blob_limit=BLOB_LIMIT,
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
        value_limit=VALUE_LIMIT,
        blob_limit=BLOB_LIMIT,
    )
    assert len(log) < BLOB_LIMIT + 100
    assert log.rstrip().endswith("→ success")


def test_elision_handles_non_json_arguments():
    """Anything that doesn't parse as a JSON object is opaque text."""
    log = format_tool_calls(
        [{"name": "odd", "arguments": "z" * 900, "success": True}],
        value_limit=VALUE_LIMIT,
        blob_limit=BLOB_LIMIT,
    )
    assert "…" in log
    assert len(log) < 300


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
        value_limit=VALUE_LIMIT,
        blob_limit=BLOB_LIMIT,
    )
    assert "→ error:" in log
    assert len(log) < 400
