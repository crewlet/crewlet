"""Per-turn iteration ledger -- what earlier ``self_iterate`` rounds did.

Each Plan/Execute/Review iteration runs on a **fresh LLM conversation**:
Plan and Execute are rebuilt from ``[system, user]`` every round and
``turn.plan_tool_executions`` is reset per iteration.  Without a record
kept outside those conversations, a ``self_iterate`` round starts blind
-- it cannot tell that iteration 1 already posted to Slack, so it plans
the post again and the side effect fires twice.

This module is that record.  :class:`IterationRecord` is an engine-built
snapshot appended once per loop-back; :func:`render_iteration_ledger`
turns the accumulated records into the prose block Plan, Execute, and
Review all read.

Two properties matter and are the reason this is code and not another
aux-LLM summarisation pass:

- **It cannot forget.**  The whole point is to stop a duplicate external
  side effect; a summariser that drops one line re-creates exactly the
  bug.  The reviewer's own prose (``ReviewOutcome.completed_work``) adds
  the semantic layer on top, but it is never the only carrier -- notably
  on the engine's post-Review ``done`` -> ``self_iterate`` override,
  where Review decided ``done`` and therefore wrote no such prose.
- **It matches how Review already judges.**  ``REVIEW_HEADER`` tells the
  reviewer the tool logs are the evidence and the narration is not; the
  ledger extends that same stance across iterations.

**Reads are marked, not merged with writes.**  Tool *results* are
deliberately not carried across iterations (``PLAN_HEADER`` says the same
about Plan results reaching Execute), so a read the next round needs
must be re-run.  Telling it "do not repeat" a ``jira_get_issue`` would
push it to fabricate the data instead.  Records therefore carry the
positively-known read names the engine already resolves from MCP
annotations for its delivery gate, and reads render with a ``(read)``
marker so the prose can permit re-running exactly those.

Deliberately free of ``crewlet.agent`` imports so
:mod:`crewlet.agent.turn_context` can hold records without a cycle;
callers pass their own ``skip_names`` (e.g. ``PLAN_META_TOOL_NAMES``).
"""

from __future__ import annotations

import json
from collections.abc import Collection, Iterable, Sequence
from dataclasses import dataclass
from typing import Any

# --- ledger elision budgets ------------------------------------------
# Applied ONLY to the cross-iteration ledger, never to Review's
# single-iteration evidence log (which stays verbatim by contract).
#
# ONE principle decides every number below: **elide payloads, never
# structure.**  A payload (a message body, page HTML, a diff) is
# unbounded, is re-authored from the plan next round, and can never
# answer the ledger's question -- so carrying it only buries the two
# lines that can.  Structure (the plan's steps, the draft under review,
# the reviewer's correction) is bounded in practice and is exactly what
# the next round has to act on, so it is cut only as a guard against
# pathological output, never as routine trimming.
#
# Everything here is a *guard*, not a diet.  Prompt caching keys on the
# system+tools prefix, which the ledger never touches, so the cost of a
# larger block is small; the reason to bound it at all is that an
# unbounded block eats the turn's own token budget (``PhaseBudget``) and
# buries the signal.

# Argument VALUES.  Identifiers are what say WHICH delivery fired --
# channel names, issue keys, page ids, handles, thread timestamps, and
# URLs -- and a long Confluence/GitHub URL with query params runs to
# ~180 chars, so 200 keeps the whole discriminator while still cutting
# message bodies and HTML by an order of magnitude.
LEDGER_VALUE_LIMIT = 200
# Backstop for a call carrying many arguments: even with every value
# elided, ~40 keys is still a wall of text.  800 chars holds roughly a
# dozen identifier-shaped arguments -- more than any real delivery tool
# takes.  Enforced by DROPPING WHOLE KEYS, never by cutting the
# serialised object -- see :func:`_fit_arguments`.
LEDGER_BLOB_LIMIT = 800
# The previous plan's steps.  A realistic 6-step plan renders ~850
# chars, so 1200 covers ~8 steps: past anything a planner emits, while
# still bounding a runaway 20-step plan.  Cutting lower chopped a
# routine plan mid-step, which reads as a SHORTER plan rather than a
# truncated one -- worse than omitting it.
LEDGER_PLAN_SUMMARY_LIMIT = 1200
# The draft Review judged.  2000 matches ``review.py``'s own
# ``execute_summary=(execute_result.text or "(empty)")[:2000]`` -- the
# same content answering the same question.  The next round has to
# improve that draft, so anything Review had enough of to judge, Plan
# needs enough of to extend rather than rewrite from scratch.
LEDGER_ARTIFACT_LIMIT = 2000
# Reviewer-authored prose (``notes`` / ``completed_work``).  ``notes``
# is the correction the next round acts on and the ledger is now its
# only carrier, so trimming it mid-instruction would lose engine-
# critical content; it gets the same 2000 as the artifact.  This is a
# pathological-output guard for a verbose model, not a routine trim --
# the ask is one or two sentences.
LEDGER_NOTE_LIMIT = 2000
# Cap on rendered tool-call lines per phase per iteration.  Execute's
# base round cap is 20 and its extension ceiling 40, so a busy iteration
# can log dozens of calls.  Only positively-known READS are ever dropped
# to fit: a write is the whole reason the ledger exists and is never
# omitted, however many there are.  12 covers the recon a normal round
# does while keeping one iteration's block skimmable.
LEDGER_MAX_READ_CALLS = 12


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
    ledger line that kept a 400-char message body but lost ``channel``
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


def _fit_arguments(elided: dict[str, Any], blob_limit: int) -> str:
    """Serialise *elided* within *blob_limit* by dropping whole keys.

    Cutting the serialised string instead would remove whichever keys
    sort last -- and with several bulky values even a fully per-value-
    elided object can exceed the blob budget, so a plain string cut
    silently discards the trailing identifiers.  That is the precise
    failure per-value elision exists to prevent, re-introduced one step
    later.

    Keys are therefore admitted shortest-value-first (identifiers are
    the short ones, payload bodies the long ones), re-serialised in the
    caller's original key order for readability, and any remainder is
    reported as ``+N more`` so a trimmed line never reads as complete.
    """
    rendered = json.dumps(elided, ensure_ascii=False)
    if blob_limit <= 0 or len(rendered) <= blob_limit:
        return rendered

    def _cost(key: str) -> int:
        return len(json.dumps({key: elided[key]}, ensure_ascii=False))

    kept: list[str] = []
    for key in sorted(elided, key=_cost):
        candidate = {k: elided[k] for k in [*kept, key]}
        if kept and len(json.dumps(candidate, ensure_ascii=False)) > blob_limit:
            break
        kept.append(key)
    keep = set(kept)
    ordered = {k: v for k, v in elided.items() if k in keep}
    out = json.dumps(ordered, ensure_ascii=False)
    dropped = len(elided) - len(ordered)
    return f"{out} +{dropped} more" if dropped else out


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
    if not isinstance(parsed, dict):
        return _elide(text, blob_limit or value_limit)
    elided = {key: _elide_value(val, value_limit) for key, val in parsed.items()}
    return _fit_arguments(elided, blob_limit)


def format_tool_calls(
    executions: Iterable[dict[str, Any]],
    *,
    skip_names: Collection[str] = (),
    read_only_names: Collection[str] = (),
    value_limit: int = 0,
    blob_limit: int = 0,
    max_read_calls: int = 0,
) -> str:
    """Evidence-only summary of tool calls.

    Each line: ``- <tool>(<args>) -> success | error: <error>``, with a
    trailing ``(read)`` when the tool is in ``read_only_names``.  Empty
    input (or input entirely in ``skip_names``) returns ``"(none)"`` so a
    reader sees an explicit "no action taken" signal rather than an
    absent section.

    ``value_limit`` / ``blob_limit`` default to 0 -- no truncation, the
    contract Review's single-iteration evidence log depends on.  The
    cross-iteration ledger passes the ``LEDGER_*`` budgets instead.

    ``max_read_calls`` (0 = unbounded) caps how many ``(read)`` lines
    render, reporting the remainder.  Only reads are ever dropped: a
    write is the whole reason this record exists.

    ``success is False`` marks an explicit failure; the ``Unknown tool:``
    branch covers entries where the loop returned the error string but
    never set ``success``.
    """
    skip = set(skip_names)
    reads = set(read_only_names)
    lines: list[str] = []
    reads_rendered = 0
    reads_dropped = 0
    for exe in executions:
        name = exe.get("name", "?")
        if name in skip:
            continue
        is_read = name in reads
        if is_read and max_read_calls > 0 and reads_rendered >= max_read_calls:
            reads_dropped += 1
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
        marker = " (read)" if is_read else ""
        lines.append(f"- {name}({args_str}) → {outcome}{marker}")
        if is_read:
            reads_rendered += 1
    if reads_dropped:
        lines.append(f"- (+{reads_dropped} further read call(s) omitted)")
    return "\n".join(lines) if lines else "(none)"


@dataclass(frozen=True)
class IterationRecord:
    """One completed Plan -> Execute -> Review round of a single turn.

    Appended by ``TurnEngine`` immediately before a ``self_iterate``
    loops back to Plan, so the record is a closed snapshot: the phases
    it describes have all finished and none of them will run again.
    Every record describes a ``self_iterate`` round -- the ``done`` /
    ``failed`` branches end the turn instead of appending -- so the
    review decision is implied and is not stored.
    """

    iteration: int
    plan_summary: str = ""
    plan_tool_calls: tuple[dict[str, Any], ...] = ()
    execute_tool_calls: tuple[dict[str, Any], ...] = ()
    read_only_names: tuple[str, ...] = ()
    """Names among the calls above that MCP annotations positively mark
    read-only, resolved by the engine's delivery gate.  Reads render
    with a ``(read)`` marker so the next round is told it may re-run
    them -- their results are not carried forward."""

    execute_text: str = ""
    review_notes: str = ""
    completed_work: str = ""
    """Reviewer-authored prose naming what already landed.  Empty
    whenever Review itself never chose ``self_iterate`` -- above all on
    the engine's post-Review ``done`` -> ``self_iterate`` override,
    which is precisely why the tool-call lists above carry the
    guarantee and this field only enriches it."""

    def to_dict(self) -> dict[str, Any]:
        """JSON-safe form, for persisting across a sandbox suspend.

        A detached ``run_sandbox`` ends the turn and the completion runs
        a NEW ``run_turn``; without round-tripping the ledger through
        ``pending_sandbox_run.execute_state`` the resumed turn would
        forget every earlier round and could re-fire its deliveries.
        """
        return {
            "iteration": self.iteration,
            "plan_summary": self.plan_summary,
            "plan_tool_calls": list(self.plan_tool_calls),
            "execute_tool_calls": list(self.execute_tool_calls),
            "read_only_names": list(self.read_only_names),
            "execute_text": self.execute_text,
            "review_notes": self.review_notes,
            "completed_work": self.completed_work,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> IterationRecord:
        """Rebuild from :meth:`to_dict`, tolerating missing keys.

        Rows persisted before the ledger existed decode to an empty
        record rather than raising, so an in-flight sandbox run started
        by an older engine still resumes.
        """
        return cls(
            iteration=int(data.get("iteration", 0) or 0),
            plan_summary=str(data.get("plan_summary", "") or ""),
            plan_tool_calls=tuple(data.get("plan_tool_calls") or ()),
            execute_tool_calls=tuple(data.get("execute_tool_calls") or ()),
            read_only_names=tuple(data.get("read_only_names") or ()),
            execute_text=str(data.get("execute_text", "") or ""),
            review_notes=str(data.get("review_notes", "") or ""),
            completed_work=str(data.get("completed_work", "") or ""),
        )


def render_iteration_ledger(
    records: Sequence[IterationRecord],
    *,
    skip_names: Collection[str] = (),
) -> str:
    """Render accumulated records as the shared prior-work block.

    Returns ``""`` when there is nothing to show (the first iteration of
    every turn), so callers can drop the whole section rather than emit
    an empty heading.  Meta-tool calls are filtered from BOTH phases:
    they are never a delivery, so in a ledger whose only job is
    "what already happened that matters" they are pure noise.

    Section headings are the caller's -- Plan/Execute frame this as
    "already done, do not repeat" while Review frames it as duplicate-
    delivery evidence -- so engine prose stays in
    :mod:`crewlet.agent.prompts`.
    """
    if not records:
        return ""
    blocks: list[str] = []
    for rec in records:
        lines = [f"### Iteration {rec.iteration}"]
        if rec.plan_summary:
            lines.append(
                "Planned: " + _elide(rec.plan_summary, LEDGER_PLAN_SUMMARY_LIMIT)
            )
        for label, calls in (
            ("Plan called:", rec.plan_tool_calls),
            ("Execute called:", rec.execute_tool_calls),
        ):
            lines.append(label)
            lines.append(
                format_tool_calls(
                    calls,
                    skip_names=skip_names,
                    read_only_names=rec.read_only_names,
                    value_limit=LEDGER_VALUE_LIMIT,
                    blob_limit=LEDGER_BLOB_LIMIT,
                    max_read_calls=LEDGER_MAX_READ_CALLS,
                )
            )
        if rec.execute_text:
            lines.append("Produced: " + _elide(rec.execute_text, LEDGER_ARTIFACT_LIMIT))
        if rec.completed_work:
            lines.append(
                "Reviewer, on what already landed: "
                + _elide(rec.completed_work, LEDGER_NOTE_LIMIT)
            )
        if rec.review_notes:
            lines.append(
                "Reviewer's correction: " + _elide(rec.review_notes, LEDGER_NOTE_LIMIT)
            )
        blocks.append("\n".join(lines))
    return "\n\n".join(blocks)
