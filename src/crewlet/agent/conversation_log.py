"""Cross-turn conversation ledger -- what this seat already did here.

:mod:`crewlet.agent.iteration_log` keeps a turn honest across its own
``self_iterate`` rounds.  This module does the same job across *turns of
one conversation*: the second comment on a Jira issue, the reply three
days later in a Slack thread.  Same doctrine, one scope wider.

Why a ledger and not a transcript.  The engine can already round-trip a
whole LLM conversation -- ``pending_sandbox_run.execute_state`` persists
the full ``Message`` list, signed thinking blocks included, and splices
it back into a running loop.  That is right for a *parked* turn, whose
dangling tool call is waiting for one answer.  It is wrong here: a
conversation's next turn arrives against a thread that has MOVED, and
replaying raw prior context invites acting on state that is no longer
true.  So this carries the same thing the iteration ledger carries --
plan, calls, artifact, verdict -- deliberately elided by the same
budgets, plus the two facts a cross-turn reader needs that a within-turn
one does not: who said what to trigger it, and what the seat finally
replied.

**Reads stay marked, never merged.**  Inherited wholesale from the
iteration ledger: tool *results* are not carried, reads render with a
``(read)`` marker, and the header tells the model it may re-run exactly
those.  Across turns the rule is stronger, not weaker -- a read from
last Tuesday is stale by construction.

Structured entries rather than prose because the write path must not
depend on an LLM: a summariser that drops the line naming the reply the
seat already sent re-creates the duplicate-answer bug in a place where
nothing else can catch it.

Free of ``crewlet.agent`` imports beyond :mod:`iteration_log`, so the
turn context and the API layer can both hold entries without a cycle.
"""

from __future__ import annotations

from collections.abc import Collection, Sequence
from dataclasses import dataclass
from typing import Any

from crewlet.agent.iteration_log import (
    LEDGER_ARTIFACT_LIMIT,
    LEDGER_BLOB_LIMIT,
    LEDGER_MAX_READ_CALLS,
    LEDGER_NOTE_LIMIT,
    LEDGER_PLAN_SUMMARY_LIMIT,
    LEDGER_VALUE_LIMIT,
    format_tool_calls,
)

# The planner's own stated reasoning, carried so a later turn inherits
# WHY rather than only what.  600 chars is two or three sentences: the
# useful residue of a phase's thinking, at a fraction of the cost of
# replaying it.  (Raw extended-thinking cannot be replayed anyway --
# only its flattened ``<think>`` text is persisted, and Anthropic
# refuses replayed thinking without the signatures nothing stores.)
SESSION_REASONING_LIMIT = 600
# What triggered the turn: sender plus the opening of what they said.
# Enough to recognise the message in a thread, never the whole body --
# the body is re-readable on the surface it came from, and the point of
# the entry is what the SEAT did about it.
SESSION_TRIGGER_LIMIT = 400
# The reply the seat actually sent.  Shares the artifact budget: it is
# the same content Review judged, answering the same question.
SESSION_REPLY_LIMIT = LEDGER_ARTIFACT_LIMIT


def _elide(text: str, limit: int) -> str:
    """Trim *text* to *limit* chars with a visible ellipsis marker."""
    if limit <= 0 or len(text) <= limit:
        return text
    return text[:limit].rstrip() + "…"


@dataclass(frozen=True)
class SessionEntry:
    """One completed turn of one conversation.

    Built by ``TurnEngine`` at turn end from data already in hand -- no
    LLM call -- and stored as the JSONB payload of a
    ``conversation_sessions`` row.
    """

    turn_id: str = ""
    at: str = ""
    """ISO-8601 completion time, rendered so the next turn can tell
    "ten minutes ago" from "last Tuesday" -- the difference between a
    live exchange and a thread that has moved on."""

    trigger: str = ""
    plan_summary: str = ""
    plan_reasoning: str = ""
    tool_calls: str = ""
    """Pre-rendered tool-call lines.  Rendered at WRITE time, so the
    elision budgets are applied once against the arguments the engine
    actually saw, and a reader (the prompt, the dashboard) never needs
    the raw execution dicts."""

    reply: str = ""
    decision: str = ""
    review_notes: str = ""
    completed_work: str = ""

    def to_dict(self) -> dict[str, Any]:
        """JSON-safe form, for the ``entry`` JSONB column."""
        return {
            "turn_id": self.turn_id,
            "at": self.at,
            "trigger": self.trigger,
            "plan_summary": self.plan_summary,
            "plan_reasoning": self.plan_reasoning,
            "tool_calls": self.tool_calls,
            "reply": self.reply,
            "decision": self.decision,
            "review_notes": self.review_notes,
            "completed_work": self.completed_work,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> SessionEntry:
        """Rebuild from :meth:`to_dict`, tolerating missing keys.

        Same stance as :meth:`IterationRecord.from_dict`: a row written
        by an older engine decodes to less context, never to an
        exception.  Losing a field costs the next turn some history;
        raising would cost it the whole turn.
        """
        return cls(
            turn_id=str(data.get("turn_id", "") or ""),
            at=str(data.get("at", "") or ""),
            trigger=str(data.get("trigger", "") or ""),
            plan_summary=str(data.get("plan_summary", "") or ""),
            plan_reasoning=str(data.get("plan_reasoning", "") or ""),
            tool_calls=str(data.get("tool_calls", "") or ""),
            reply=str(data.get("reply", "") or ""),
            decision=str(data.get("decision", "") or ""),
            review_notes=str(data.get("review_notes", "") or ""),
            completed_work=str(data.get("completed_work", "") or ""),
        )


def build_session_entry(
    *,
    turn_id: str,
    at: str,
    trigger: str,
    plan_summary: str,
    plan_reasoning: str,
    tool_executions: Sequence[dict[str, Any]],
    read_only_names: Collection[str],
    skip_names: Collection[str],
    reply: str,
    decision: str,
    review_notes: str = "",
    completed_work: str = "",
) -> SessionEntry:
    """Assemble one entry, applying every budget once at write time."""
    return SessionEntry(
        turn_id=turn_id,
        at=at,
        trigger=_elide(trigger, SESSION_TRIGGER_LIMIT),
        plan_summary=_elide(plan_summary, LEDGER_PLAN_SUMMARY_LIMIT),
        plan_reasoning=_elide(plan_reasoning, SESSION_REASONING_LIMIT),
        tool_calls=format_tool_calls(
            tool_executions,
            skip_names=skip_names,
            read_only_names=read_only_names,
            value_limit=LEDGER_VALUE_LIMIT,
            blob_limit=LEDGER_BLOB_LIMIT,
            max_read_calls=LEDGER_MAX_READ_CALLS,
        ),
        reply=_elide(reply, SESSION_REPLY_LIMIT),
        decision=decision,
        review_notes=_elide(review_notes, LEDGER_NOTE_LIMIT),
        completed_work=_elide(completed_work, LEDGER_NOTE_LIMIT),
    )


def render_conversation_history(
    entries: Sequence[SessionEntry],
    *,
    max_entries: int = 0,
    max_chars: int = 0,
) -> str:
    """Render prior turns of this conversation as the injected block.

    Returns ``""`` when there is nothing to show -- the first turn of
    every conversation -- so the caller drops the whole section rather
    than emitting an empty heading.

    ``max_entries`` keeps the newest N; ``max_chars`` then drops from the
    OLDEST end until the block fits.  Oldest-first because recency is
    what a follow-up turn needs: the message it is answering is the
    newest one, and the turn before it is the one most likely to have
    already answered it.  Both default to 0 (unbounded) so a caller
    reading for display -- the dashboard -- gets everything.

    Section headings are the caller's, matching
    :func:`render_iteration_ledger`: engine prose lives in
    :mod:`crewlet.agent.prompts`.
    """
    if not entries:
        return ""
    selected = list(entries)
    if max_entries > 0:
        selected = selected[-max_entries:]
    blocks = [_render_entry(entry) for entry in selected]
    if max_chars > 0:
        while len(blocks) > 1 and len("\n\n".join(blocks)) > max_chars:
            blocks.pop(0)
    return "\n\n".join(blocks)


def _render_entry(entry: SessionEntry) -> str:
    """One entry as prose.

    Second person throughout ("You planned", "You replied") because the
    reader is the same seat on a later turn: the block is its own past,
    not a report about someone else.
    """
    head = f"### {entry.at}" if entry.at else "### Earlier turn"
    if entry.turn_id:
        head += f" (turn {entry.turn_id[:8]})"
    lines = [head]
    if entry.trigger:
        lines.append(f"Triggered by: {entry.trigger}")
    if entry.plan_summary:
        lines.append(f"You planned: {entry.plan_summary}")
    if entry.plan_reasoning:
        lines.append(f"Your reasoning: {entry.plan_reasoning}")
    if entry.tool_calls:
        lines.append("You called:")
        lines.append(entry.tool_calls)
    if entry.reply:
        lines.append(f"You replied: {entry.reply}")
    if entry.completed_work:
        lines.append(f"Reviewer, on what landed: {entry.completed_work}")
    if entry.review_notes:
        lines.append(f"Reviewer's correction: {entry.review_notes}")
    if entry.decision and entry.decision != "done":
        lines.append(f"Turn ended: {entry.decision}")
    return "\n".join(lines)


__all__ = [
    "SESSION_REASONING_LIMIT",
    "SESSION_REPLY_LIMIT",
    "SESSION_TRIGGER_LIMIT",
    "SessionEntry",
    "build_session_entry",
    "render_conversation_history",
]
