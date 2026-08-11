"""Tests for the personal-memory digest used by the Plan-phase prompt.

Covers:
- Candidate filtering (memory_long / memory_short kinds, TTL expiry).
- Aux-LLM relevance filter happy path (returns indices, renders bullets).
- Aux-LLM failure paths (empty content / unparseable / raises) -- all
  must render nothing rather than fall back to recency: a permissive
  recency fallback would leak per-subject
  memories on cap hits.
- Sender enrichment: ``Current sender:`` line in the filter prompt
  when an :class:`InboundInteraction` is supplied.
- TTL rendering on SHORT entries.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime, timedelta
from typing import Any

from crewlet.learning.interaction import CanonicalIdentity, InboundInteraction
from crewlet.learning.personal_memory import (
    DEFAULT_FILTER_OUTPUT_TOKENS,
    EMPTY_FILTER_HINT,
    KIND_LONG,
    KIND_SHORT,
    _build_filter_user_prompt,
    _parse_indices,
    fetch_existing_memories,
    fetch_personal_memory_block,
)
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import Completion


class _ScriptedProvider:
    def __init__(self, completions: list[Completion]) -> None:
        self._completions = list(completions)
        self.calls: list[dict[str, Any]] = []
        self.model = "scripted"

    async def complete(
        self, messages, tools=None, temperature=0.7, max_tokens=None, tool_choice=None
    ):
        self.calls.append({"messages": messages})
        if self._completions:
            return self._completions.pop(0)
        return Completion(content="[]")


class _DiaryStub:
    """Stand-in for :class:`~crewlet.learning.diary.AgentDiary`.

    ``list_for_agent`` returns the docs the test passed in, filtered
    by ``agent_id`` and (when ``include_expired=False``, the default)
    by TTL -- mirrors the real diary's contract.  ``mark_retrieved``
    is recorded so tests can assert the read-side telemetry path
    fired on selected indices.
    """

    def __init__(self, docs: list[dict[str, Any]]) -> None:
        self.docs = list(docs)
        self.marked_retrieved: list[str] = []

    async def list_for_agent(
        self,
        agent_id: str,
        *,
        limit: int = 100,
        include_expired: bool = False,
    ) -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = []
        for d in self.docs:
            if d.get("agent_id") and d["agent_id"] != agent_id:
                continue
            if not include_expired:
                meta = d.get("metadata") or {}
                ttl = meta.get("ttl_until") if isinstance(meta, dict) else None
                if ttl:
                    try:
                        expiry = datetime.fromisoformat(ttl)
                    except ValueError:
                        expiry = None
                    if expiry is not None and expiry < datetime.now(UTC):
                        continue
            out.append(d)
            if len(out) >= limit:
                break
        return out

    async def mark_retrieved(self, doc_id: str) -> None:
        self.marked_retrieved.append(str(doc_id))

    async def mark_retrieved_many(self, doc_ids: list[str]) -> None:
        for doc_id in doc_ids:
            self.marked_retrieved.append(str(doc_id))

    async def search_for_agent(
        self,
        agent_id: str,
        *,
        query: str,
        limit: int = 50,
        include_expired: bool = False,
        min_similarity: float = 0.0,
    ) -> list[dict[str, Any]]:
        # The relevance-filter tests in this file focus on the recency
        # half of the hybrid candidate selection; vector search is
        # exercised by the dedicated hybrid tests at the bottom of the
        # file.  Default to no vector hits so callers fall through to
        # list_for_agent.
        return []


def _doc(
    *,
    doc_id: str,
    content: str,
    kind: str = KIND_LONG,
    ttl_until: datetime | None = None,
    agent_id: str = "agent-1",
) -> dict[str, Any]:
    meta: dict[str, Any] = {"kind": kind}
    if ttl_until is not None:
        meta["ttl_until"] = ttl_until.isoformat()
    return {
        "id": doc_id,
        "agent_id": agent_id,
        "content": content,
        "metadata": meta,
    }


def _role() -> Role:
    return Role(name="Engineer", handle="eng", llm="cheap", llm_auxiliary="cheap")


# --- Index parsing ------------------------------------------------------


def test_parse_indices_pure_json_array() -> None:
    assert _parse_indices("[3, 0, 7]") == [3, 0, 7]


def test_parse_indices_with_prose_wrapping() -> None:
    text = "Sure! Here's the answer: [1, 2, 4]\nThanks."
    assert _parse_indices(text) == [1, 2, 4]


def test_parse_indices_dedupes_and_caps_at_eight() -> None:
    # Duplicates dropped; cap at 8.
    assert _parse_indices("[1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10]") == [
        1,
        2,
        3,
        4,
        5,
        6,
        7,
        8,
    ]


def test_parse_indices_returns_empty_on_garbage() -> None:
    assert _parse_indices("nope") == []
    assert _parse_indices("") == []


def test_parse_indices_handles_non_int_entries() -> None:
    assert _parse_indices('[1, "two", 3.0, null]') == [1, 3]


# --- End-to-end fetch ---------------------------------------------------


async def test_fetch_returns_empty_when_no_knowledge() -> None:
    out = await fetch_personal_memory_block(
        diary=None,
        agent_id="agent-1",
        role=_role(),
        task_description="some task",
    )
    assert out == ""


async def test_fetch_returns_empty_when_no_task() -> None:
    knowledge = _DiaryStub([_doc(doc_id="d1", content="x")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="",
    )
    assert out == ""


async def test_fetch_filters_to_memory_kinds_only() -> None:
    """Non-memory rows (e.g. onboarding markers) must not surface even
    when the filter approves all candidates."""
    knowledge = _DiaryStub(
        [
            _doc(doc_id="d1", content="A real memory.", kind=KIND_LONG),
            _doc(doc_id="d2", content="Marker doc.", kind="onboarding_marker"),
        ]
    )
    # LLM picks index 0 (the only candidate -- the marker was filtered
    # out before the LLM saw anything).
    provider = _ScriptedProvider([Completion(content="[0]")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="any task",
        llm_providers={"cheap": provider},
    )
    assert "real memory" in out.lower()
    assert "Marker" not in out
    # The marker doc was excluded at candidate-selection time, so the
    # LLM only ever saw one numbered entry.
    user_msg = provider.calls[0]["messages"][1].content
    assert "0. A real memory." in user_msg
    assert "Marker" not in user_msg


async def test_fetch_filters_expired_short_entries() -> None:
    """SHORT memories past their ttl_until must be excluded from the
    candidate list before the filter even sees them."""
    expired = datetime.now(UTC) - timedelta(days=1)
    fresh = datetime.now(UTC) + timedelta(days=10)
    knowledge = _DiaryStub(
        [
            # Newest first (mirrors real provider's DESC ordering).
            _doc(
                doc_id="d-fresh",
                content="Fresh ephemeral fact.",
                kind=KIND_SHORT,
                ttl_until=fresh,
            ),
            _doc(
                doc_id="d-old",
                content="Stale ephemeral fact.",
                kind=KIND_SHORT,
                ttl_until=expired,
            ),
        ]
    )
    provider = _ScriptedProvider([Completion(content="[0]")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="any task",
        llm_providers={"cheap": provider},
    )
    assert "Stale" not in out
    assert "Fresh" in out
    # SHORT rendering tags the expiry date.
    assert "(until " in out
    # The expired row never reached the filter prompt either.
    user_msg = provider.calls[0]["messages"][1].content
    assert "Stale" not in user_msg


async def test_fetch_uses_aux_llm_filter_when_provider_wired() -> None:
    """LLM picks indices [1, 2]; only those bullets render in order."""
    # Docs passed newest-first (matches list_documents contract).
    knowledge = _DiaryStub(
        [
            _doc(doc_id="d0", content="Memory A — irrelevant"),
            _doc(doc_id="d1", content="Memory B — relevant"),
            _doc(doc_id="d2", content="Memory C — also relevant"),
        ]
    )
    provider = _ScriptedProvider([Completion(content=json.dumps([1, 2]))])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="please do the thing",
        llm_providers={"cheap": provider},
    )
    lines = out.split("\n")
    # candidates list = [d0, d1, d2]; LLM picks indices [1, 2] →
    # Memory B and Memory C; Memory A is dropped.
    assert any("Memory B" in line for line in lines)
    assert any("Memory C" in line for line in lines)
    assert not any("Memory A" in line for line in lines)
    # Order matches LLM priority: index 1 (B) before index 2 (C).
    b_pos = next(i for i, line in enumerate(lines) if "Memory B" in line)
    c_pos = next(i for i, line in enumerate(lines) if "Memory C" in line)
    assert b_pos < c_pos
    assert len(provider.calls) == 1


async def test_fetch_renders_nothing_when_aux_llm_unparseable() -> None:
    """LLM returns garbage that isn't an empty array → render nothing.

    Falling back to recency-sorted rendering
    of ALL candidates would silently bypass the filter and leak
    per-subject memories.  The contract: filter failure = no memory.
    """
    knowledge = _DiaryStub(
        [
            _doc(doc_id="d-newer", content="Newer memory"),
            _doc(doc_id="d-older", content="Older memory"),
        ]
    )
    provider = _ScriptedProvider([Completion(content="this is not JSON at all")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="task",
        llm_providers={"cheap": provider},
    )
    assert out == ""


async def test_fetch_renders_nothing_when_aux_llm_returns_empty_content() -> None:
    """Cap-hit symptom: thinking-capable model burned the output budget
    on internal reasoning and emitted no visible text.

    Critically: must NOT fall back to recency.  Empty content is the
    exact failure mode that motivated removing the recency fallback.
    """
    knowledge = _DiaryStub(
        [
            _doc(doc_id="d0", content="Per-subject fact"),
            _doc(doc_id="d1", content="Another per-subject fact"),
        ]
    )
    # ``content=""`` simulates the cap-hit / pure-thinking response
    # shape the trace surfaced.
    provider = _ScriptedProvider([Completion(content="")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="task",
        llm_providers={"cheap": provider},
    )
    assert out == ""


async def test_fetch_renders_nothing_when_aux_llm_raises() -> None:
    """LLM exception → render nothing (was: fall back to recency)."""

    class _Boom:
        model = "boom"

        async def complete(self, *args, **kwargs):
            raise RuntimeError("transient outage")

    knowledge = _DiaryStub([_doc(doc_id="d0", content="A memory")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="task",
        llm_providers={"cheap": _Boom()},  # type: ignore[dict-item]
    )
    assert out == ""


async def test_fetch_renders_empty_filter_hint_when_aux_llm_returns_empty_array() -> (
    None
):
    """LLM emits ``[]`` while the agent has candidates -> render the
    ``EMPTY_FILTER_HINT`` nudge instead of going silently empty.  Lets
    the planner distinguish "no memories exist" from "memories exist
    but the turn-start filter didn't surface them" and decide whether
    to call ``refresh_memory`` after gathering context.
    """
    from crewlet.learning.personal_memory import EMPTY_FILTER_HINT

    knowledge = _DiaryStub(
        [
            _doc(doc_id="d-newer", content="Newer memory"),
            _doc(doc_id="d-older", content="Older memory"),
        ]
    )
    provider = _ScriptedProvider([Completion(content="[]")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="task",
        llm_providers={"cheap": provider},
    )
    assert out == EMPTY_FILTER_HINT
    assert "refresh_memory" in out  # the hint names the escape hatch


async def test_fetch_renders_nothing_when_no_llm_providers() -> None:
    """Filter unavailable -> no memory shown (was: recency fallback)."""
    knowledge = _DiaryStub(
        [_doc(doc_id="d0", content="Any memory")],
    )
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="task",
        llm_providers=None,
    )
    assert out == ""


async def test_fetch_renders_nothing_when_no_role() -> None:
    """Role missing -> filter unavailable -> no memory shown."""
    knowledge = _DiaryStub([_doc(doc_id="d0", content="Any memory")])
    provider = _ScriptedProvider([Completion(content="[0]")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=None,
        task_description="task",
        llm_providers={"cheap": provider},
    )
    assert out == ""
    # And the LLM was never called -- no role means no provider
    # resolution attempt.
    assert provider.calls == []


async def test_fetch_respects_char_budget_during_render() -> None:
    """Long bullets get dropped once the budget is exhausted, even on
    the LLM-approved path."""
    big = "X" * 500
    knowledge = _DiaryStub(
        [
            _doc(doc_id="d-newest", content=f"Memory 3 {big}"),
            _doc(doc_id="d-mid", content=f"Memory 2 {big}"),
            _doc(doc_id="d-oldest", content=f"Memory 1 {big}"),
        ]
    )
    # LLM picks all three in priority order.
    provider = _ScriptedProvider([Completion(content="[0, 1, 2]")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="task",
        llm_providers={"cheap": provider},
        char_budget=600,
    )
    # Budget 600 chars → only the highest-priority one fits.
    bullets = [line for line in out.split("\n") if line.strip()]
    assert len(bullets) == 1
    assert "Memory 3" in bullets[0]


async def test_fetch_empty_when_no_memory_rows_match() -> None:
    knowledge = _DiaryStub([_doc(doc_id="d0", content="x", kind="onboarding_marker")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="task",
    )
    assert out == ""


# --- Thin-trigger gate ---------------------------------------------------


async def test_fetch_skips_aux_call_on_thin_trigger() -> None:
    """When the trigger is a bare pointer (a webhook naming a
    thing-that-changed), running the filter against it is
    near-guaranteed low-value -- skip the aux LLM call entirely and
    nudge the planner to refresh after recon.  The agent HAS memory
    rows, so the hint is the right output."""
    knowledge = _DiaryStub([_doc(doc_id="d1", content="Stakeholder X prefers digests")])
    provider = _ScriptedProvider([Completion(content="[0]")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="A Jira event has been routed to you. POC-518 ...",
        llm_providers={"cheap": provider},
        trigger_requires_recon=True,
    )
    assert out == EMPTY_FILTER_HINT
    # The aux LLM was never called -- the gate fired before it.
    assert provider.calls == []


async def test_fetch_thin_trigger_with_no_memory_rows_renders_nothing() -> None:
    """Thin trigger + empty diary -> nothing to nudge about, render
    an empty block (the no-candidates check runs before the gate)."""
    knowledge = _DiaryStub([])
    provider = _ScriptedProvider([Completion(content="[0]")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="A Jira event has been routed to you.",
        llm_providers={"cheap": provider},
        trigger_requires_recon=True,
    )
    assert out == ""
    assert provider.calls == []


async def test_fetch_rich_trigger_still_runs_aux_filter() -> None:
    """``trigger_requires_recon=False`` (the default, and what a
    self-contained trigger gets) keeps the normal aux-filter path."""
    knowledge = _DiaryStub([_doc(doc_id="d1", content="Memory B — relevant")])
    provider = _ScriptedProvider([Completion(content="[0]")])
    out = await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="full self-contained task description",
        llm_providers={"cheap": provider},
        trigger_requires_recon=False,
    )
    assert "Memory B" in out
    assert len(provider.calls) == 1  # aux filter ran


# --- Sender enrichment ---------------------------------------------------


def test_filter_user_prompt_renders_current_sender_line() -> None:
    """When an interaction with a known sender is supplied, the filter
    prompt opens with a structured ``Current sender:`` line so the LLM
    can compare per-subject memories against the sender identity."""
    interaction = InboundInteraction(
        sender=CanonicalIdentity(
            handle="miles",
            external_id="U0TESTUSER2",
            platform="slack",
            display_name="Miles",
        ),
        body="I need backend review",
    )
    rendered = _build_filter_user_prompt(
        "I need backend review",
        [
            {
                "content": "User Sam prefers terse replies",
                "metadata": {"kind": KIND_LONG},
            }
        ],
        interactions=[interaction],
    )
    assert "Current sender: Miles (slack:U0TESTUSER2)" in rendered
    # Sender header sits before the task block so the LLM reads it
    # first.
    sender_idx = rendered.index("Current sender:")
    task_idx = rendered.index("Task the agent is about to plan:")
    assert sender_idx < task_idx


def test_filter_user_prompt_omits_sender_line_when_no_interaction() -> None:
    """Internal triggers (TaskAssigned, scheduled ticks) have no
    sender -- the prompt must remain compact, with no sender line,
    so the filter still works on the task body alone."""
    rendered = _build_filter_user_prompt(
        "internal task",
        [{"content": "x", "metadata": {"kind": KIND_LONG}}],
        interactions=None,
    )
    assert "Current sender" not in rendered


def test_filter_user_prompt_handles_handle_only_sender() -> None:
    """Resolved internal-agent senders may have only a handle, no
    external_id."""
    interaction = InboundInteraction(
        sender=CanonicalIdentity(handle="agent-cto", display_name="CTO Agent"),
    )
    rendered = _build_filter_user_prompt(
        "task",
        [{"content": "x", "metadata": {"kind": KIND_LONG}}],
        interactions=[interaction],
    )
    assert "Current sender: CTO Agent" in rendered


async def test_fetch_threads_interaction_into_filter_prompt() -> None:
    """End-to-end: interaction passed to fetch must reach the LLM."""
    knowledge = _DiaryStub(
        [_doc(doc_id="d0", content="User Sam prefers terse replies")]
    )
    provider = _ScriptedProvider([Completion(content="[]")])
    interaction = InboundInteraction(
        sender=CanonicalIdentity(
            handle="miles",
            external_id="U0TESTUSER2",
            platform="slack",
            display_name="Miles",
        ),
    )
    await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="task",
        interactions=[interaction],
        llm_providers={"cheap": provider},
    )
    user_msg = provider.calls[0]["messages"][1].content
    assert "Current sender: Miles (slack:U0TESTUSER2)" in user_msg


def test_filter_system_prompt_describes_current_sender_rule() -> None:
    """The system prompt must teach the LLM how to use the
    ``Current sender:`` line, otherwise the data is wasted."""
    from crewlet.learning.personal_memory import _FILTER_SYSTEM_PROMPT

    assert "Current sender:" in _FILTER_SYSTEM_PROMPT
    assert "subject" in _FILTER_SYSTEM_PROMPT


def test_filter_system_prompt_distinguishes_operational_from_per_subject() -> None:
    """The prompt must call out that operational memories (routing,
    OOO, freezes) apply regardless of who's asking -- the
    subject-mismatch filter is for per-subject preferences only.

    Regression guard: an LLM can misread "memory names Sam, sender
    isn't Sam" and exclude a "Sam is OOO, route backend reviews
    to Maria" memory from a Miles-asks-about-backend-review task.
    The OOO routing rule is operational, not per-subject; it should
    apply.  The prompt structures the rules with explicit precedence
    and an "about-ness" framing (operational = about a situation;
    per-subject = about how to engage with someone).
    """
    from crewlet.learning.personal_memory import _FILTER_SYSTEM_PROMPT

    # Operational rule must come first (LLM tends to weight earlier
    # rules as primary).
    op_idx = _FILTER_SYSTEM_PROMPT.find("Operational context")
    sender_rule_idx = _FILTER_SYSTEM_PROMPT.find("Per-subject preferences")
    assert op_idx > 0, "operational-context rule missing"
    assert sender_rule_idx > 0, "per-subject rule missing"
    assert op_idx < sender_rule_idx, "operational rule must precede per-subject rule"

    # The "subject filter only applies to per-subject memories" idea
    # must be explicit.  Prevents LLMs from overgeneralizing the
    # exclusion to operational memories that happen to name a subject.
    assert "regardless of who's asking" in _FILTER_SYSTEM_PROMPT
    assert "Subject mismatch alone is NOT" in _FILTER_SYSTEM_PROMPT

    # The about-ness framing -- operational memories are about a
    # situation, per-subject memories are about how to engage with
    # a person.  This is the underlying principle the LLM should
    # apply.
    assert "about a situation" in _FILTER_SYSTEM_PROMPT
    assert "how to interact with" in _FILTER_SYSTEM_PROMPT


# --- Resolved-selection annotation --------------------------------------


def test_format_aux_response_appends_resolution_block_for_picked_indices() -> None:
    """The aux LLM returns indices like ``[0, 2]``; the framework
    decorates the published event's ``response`` field with a
    ``→ Resolved selection:`` block listing the actual memory text
    for each picked index so the dashboard view shows what the
    indices map to without scrolling up to the candidate list."""
    from crewlet.learning.personal_memory import _format_aux_response_with_resolution

    candidates = [
        {"content": "User Sam prefers terse replies"},
        {"content": "Auth uses JWT in the Authorization header"},
        {"content": "Sarah Chen reviews backend PRs in the morning"},
    ]
    completion = Completion(content="[0, 2]")
    out = _format_aux_response_with_resolution(completion, candidates)
    # Original LLM response preserved.
    assert "[0, 2]" in out
    # Resolution block appended in priority order.
    assert "→ Resolved selection:" in out
    assert "0. User Sam prefers terse replies" in out
    assert "2. Sarah Chen reviews backend PRs in the morning" in out
    # Index 1 was NOT picked -- it must not appear.
    assert "1. Auth uses JWT" not in out
    # Resolution block sits AFTER the LLM answer.
    assert out.index("[0, 2]") < out.index("→ Resolved selection:")


def test_format_aux_response_combines_with_reasoning_wrapper() -> None:
    """When the model emits both reasoning and an indices answer, the
    decorator builds on top of the ``<think>`` wrapping rather than
    replacing it."""
    from crewlet.learning.personal_memory import _format_aux_response_with_resolution

    candidates = [{"content": "Memory A"}]
    completion = Completion(
        content="[0]",
        reasoning_content="thinking about which memory applies",
    )
    out = _format_aux_response_with_resolution(completion, candidates)
    # Reasoning preserved in <think>, then visible answer, then resolution.
    assert "<think>thinking about which memory applies</think>" in out
    assert "[0]" in out
    assert "0. Memory A" in out
    # Order: <think> → [0] → resolution.
    assert out.index("<think>") < out.index("[0]") < out.index("→ Resolved selection:")


def test_format_aux_response_omits_resolution_when_empty_selection() -> None:
    """``[]`` is self-explanatory -- no need for an empty resolution
    block.  Just return the base format unchanged."""
    from crewlet.learning.personal_memory import _format_aux_response_with_resolution

    candidates = [{"content": "Memory A"}]
    completion = Completion(content="[]")
    out = _format_aux_response_with_resolution(completion, candidates)
    assert out == "[]"
    assert "→ Resolved selection:" not in out


def test_format_aux_response_skips_out_of_bounds_indices() -> None:
    """A buggy LLM that emits indices past the candidate list must
    not crash the renderer.  Out-of-bounds entries are silently
    skipped; valid ones still render."""
    from crewlet.learning.personal_memory import _format_aux_response_with_resolution

    candidates = [{"content": "Memory A"}]
    completion = Completion(content="[0, 5, 99]")
    out = _format_aux_response_with_resolution(completion, candidates)
    assert "0. Memory A" in out
    # No phantom bullets for the invalid indices.
    assert "5." not in out
    assert "99." not in out


def test_format_aux_response_truncates_long_memory_content() -> None:
    """A pathologically long memory entry must not bloat the dashboard
    response field."""
    from crewlet.learning.personal_memory import _format_aux_response_with_resolution

    long_text = "x" * 500
    candidates = [{"content": long_text}]
    completion = Completion(content="[0]")
    out = _format_aux_response_with_resolution(completion, candidates)
    # Truncated to ~240 chars + "..." -- well under the 500 raw size.
    assert len(out) < 400
    assert "..." in out


def test_format_aux_response_returns_base_when_response_unparseable() -> None:
    """Garbage in the response field shouldn't raise; just return
    the base format and let the dashboard show the raw text."""
    from crewlet.learning.personal_memory import _format_aux_response_with_resolution

    candidates = [{"content": "Memory A"}]
    completion = Completion(content="not JSON at all")
    out = _format_aux_response_with_resolution(completion, candidates)
    assert out == "not JSON at all"
    assert "→ Resolved selection:" not in out


def test_format_aux_response_returns_base_when_completion_empty() -> None:
    """Cap-hit / empty content (e.g. thinking model exhausted budget)
    must not produce a stray resolution block."""
    from crewlet.learning.personal_memory import _format_aux_response_with_resolution

    candidates = [{"content": "Memory A"}]
    completion = Completion(content="", reasoning_content="thinking but never finished")
    out = _format_aux_response_with_resolution(completion, candidates)
    # Reasoning surfaces in <think>, no resolution block.
    assert "<think>thinking but never finished</think>" in out
    assert "→ Resolved selection:" not in out


async def test_fetch_publishes_event_with_resolved_selection() -> None:
    """End-to-end: the personal-memory filter wires its annotator
    through to the queue.  The published ``AgentPhaseCompleted``
    event's ``response`` field carries the resolved selection block."""

    class _Queue:
        def __init__(self):
            self.events: list[tuple[str, Any]] = []

        async def publish(self, topic, event):
            self.events.append((topic, event))

    queue = _Queue()
    knowledge = _DiaryStub(
        [
            _doc(doc_id="d0", content="User Sam prefers terse replies"),
            _doc(doc_id="d1", content="Memory B"),
        ]
    )
    provider = _ScriptedProvider([Completion(content="[0]")])
    await fetch_personal_memory_block(
        diary=knowledge,  # type: ignore[arg-type]
        agent_id="agent-1",
        role=_role(),
        task_description="task",
        llm_providers={"cheap": provider},
        event_queue=queue,
    )
    # One aux event published; its response contains the resolution.
    assert len(queue.events) == 1
    _, event = queue.events[0]
    assert "[0]" in event.response
    assert "→ Resolved selection:" in event.response
    assert "0. User Sam prefers terse replies" in event.response
    assert "1. Memory B" not in event.response  # not picked


def test_default_filter_output_tokens_has_thinking_headroom() -> None:
    """Cap must be high enough that an extended-thinking model can
    actually emit the JSON array.  Sanity check that future tweaks
    don't silently regress to 200 (the value that triggered the
    leak)."""
    assert DEFAULT_FILTER_OUTPUT_TOKENS >= 1000


# ---------------------------------------------------------------------------
# Hybrid candidate selection (vector top-K union recency top-K)
# ---------------------------------------------------------------------------


async def test_fetch_existing_memories_hybrid_with_query() -> None:
    """With a query, candidate selection unions vector top-K and
    recency top-K, deduped by row id; vector hits come first."""

    class _HybridDiary:
        def __init__(self, vector_hits, recency_hits):
            self.vector_hits = vector_hits
            self.recency_hits = recency_hits
            self.search_calls: list[dict[str, Any]] = []

        async def search_for_agent(
            self,
            agent_id: str,
            *,
            query: str,
            limit: int = 50,
            include_expired: bool = False,
        ):
            self.search_calls.append({"query": query, "limit": limit})
            return list(self.vector_hits)

        async def list_for_agent(
            self,
            agent_id: str,
            *,
            limit: int = 100,
            include_expired: bool = False,
        ):
            return list(self.recency_hits)

    v_only = _doc(doc_id="v_only", content="vector-only hit")
    shared = _doc(doc_id="shared", content="appears in both pools")
    r_only = _doc(doc_id="r_only", content="recent-only row")

    diary = _HybridDiary(
        vector_hits=[v_only, shared],
        recency_hits=[shared, r_only],
    )

    out = await fetch_existing_memories(
        diary=diary,  # type: ignore[arg-type]
        agent_id="agent-1",
        query="find some hits",
    )

    # 3 unique rows; vector hits first, then recency-only (dedup).
    ids = [d["id"] for d in out]
    assert ids == ["v_only", "shared", "r_only"]
    # The query reached search_for_agent.
    assert diary.search_calls == [{"query": "find some hits", "limit": 50}]


async def test_fetch_existing_memories_no_query_skips_vector() -> None:
    """Without a query (the PersistDecider write-side dedup path),
    candidate selection is pure recency -- vector search is not called."""

    class _RecencyDiary:
        def __init__(self, rows):
            self.rows = rows
            self.search_called = False

        async def search_for_agent(self, *args, **kwargs):
            self.search_called = True
            return []

        async def list_for_agent(
            self,
            agent_id: str,
            *,
            limit: int = 100,
            include_expired: bool = False,
        ):
            return list(self.rows)

    rows = [_doc(doc_id="r1", content="row")]
    diary = _RecencyDiary(rows)

    out = await fetch_existing_memories(
        diary=diary,  # type: ignore[arg-type]
        agent_id="agent-1",
    )

    assert [d["id"] for d in out] == ["r1"]
    assert not diary.search_called


async def test_fetch_existing_memories_falls_back_when_search_raises() -> None:
    """If the diary's vector search fails (no embeddings provider,
    transient outage), recency alone still drives the candidate
    pool -- the prefetch never crashes."""

    class _BrokenSearchDiary:
        def __init__(self, rows):
            self.rows = rows

        async def search_for_agent(self, *args, **kwargs):
            raise RuntimeError("embeddings api down")

        async def list_for_agent(self, agent_id, *, limit=100, include_expired=False):
            return list(self.rows)

    rows = [_doc(doc_id="r1", content="recent row")]
    diary = _BrokenSearchDiary(rows)

    out = await fetch_existing_memories(
        diary=diary,  # type: ignore[arg-type]
        agent_id="agent-1",
        query="some query",
    )

    assert [d["id"] for d in out] == ["r1"]
