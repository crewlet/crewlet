"""Rendering of a phase's tool calls as evidence.

Review judges delivery from what a phase actually *called*, not from
what it narrated about itself (``REVIEW_HEADER`` says so explicitly), so
the tool log is the load-bearing part of its prompt.  This module owns
that rendering as one function with a truncation policy, rather than a
formatter per consumer that can drift.

Deliberately free of ``crewlet.agent`` imports so any phase module can
use it without an import cycle; callers pass their own ``skip_names``
(e.g. ``PLAN_META_TOOL_NAMES``).
"""

from __future__ import annotations

import json
from collections.abc import Collection, Iterable
from typing import Any


def _elide(text: str, limit: int) -> str:
    """Trim *text* to *limit* chars with a visible ellipsis marker."""
    if limit <= 0 or len(text) <= limit:
        return text
    return text[:limit].rstrip() + "…"


def _elide_value(value: Any, limit: int) -> Any:
    """Elide one argument VALUE, leaving its key intact.

    Per-value rather than per-blob because ``json.dumps`` preserves key
    order: capping the serialised object at N chars drops whichever keys
    sort last, and the discriminating argument (``channel``, ``key``,
    ``page_id``) is usually the SHORTEST one and can sit anywhere.  A
    log line that kept a 400-char message body but lost ``channel``
    would be worse than useless -- it would look precise while hiding
    which of two deliveries actually fired.

    Values that already fit are returned untouched so the rendered JSON
    keeps its native types.
    """
    if isinstance(value, str):
        return _elide(value, limit)
    try:
        dumped = json.dumps(value, ensure_ascii=False)
    except (TypeError, ValueError):
        dumped = str(value)
    if len(dumped) <= limit:
        return value
    return _elide(dumped, limit)


def _render_arguments(raw: Any, *, value_limit: int, blob_limit: int) -> str:
    """Render a call's ``arguments`` field, optionally eliding values.

    ``arguments`` is always a JSON string in production data (see
    ``llm_loop.run_tool_loop``).  ``value_limit <= 0`` returns it
    verbatim, which is the contract Review's evidence log relies on.
    Anything that fails to parse as a JSON object is treated as opaque
    text and elided whole.
    """
    text = str(raw if raw is not None else "")
    if value_limit <= 0:
        return text
    try:
        parsed = json.loads(text) if text else None
    except (TypeError, ValueError):
        parsed = None
    if isinstance(parsed, dict):
        rendered = json.dumps(
            {key: _elide_value(val, value_limit) for key, val in parsed.items()},
            ensure_ascii=False,
        )
    else:
        rendered = _elide(text, value_limit)
    if blob_limit > 0:
        rendered = _elide(rendered, blob_limit)
    return rendered


def format_tool_calls(
    executions: Iterable[dict[str, Any]],
    *,
    skip_names: Collection[str] = (),
    value_limit: int = 0,
    blob_limit: int = 0,
) -> str:
    """Evidence-only summary of tool calls.

    Each line: ``- <tool>(<args>) -> success | error: <error>``.  Empty
    input (or input that is entirely ``skip_names``) returns ``"(none)"``
    so a reader sees an explicit "no action taken" signal rather than an
    absent section.

    ``value_limit`` / ``blob_limit`` default to 0 -- no truncation.  That
    is the contract Review's evidence log depends on: it is what
    delivery is judged against, so it must show exactly what each call
    did.  Callers that render many calls at once pass budgets instead.

    ``success is False`` marks an explicit failure; the ``Unknown tool:``
    branch covers entries where the loop returned the error string but
    never set ``success``.
    """
    skip = set(skip_names)
    lines: list[str] = []
    for exe in executions:
        name = exe.get("name", "?")
        if name in skip:
            continue
        args_str = _render_arguments(
            exe.get("arguments", ""),
            value_limit=value_limit,
            blob_limit=blob_limit,
        )
        success = exe.get("success")
        result = str(exe.get("result", ""))
        if success is False or result.startswith("Unknown tool:"):
            outcome = f"error: {_elide(result, value_limit)}"
        else:
            outcome = "success"
        lines.append(f"- {name}({args_str}) → {outcome}")
    return "\n".join(lines) if lines else "(none)"
