"""Personal-memory digest for the Plan-phase prompt.

Surfaces an agent's own LONG / SHORT memories (written by
:class:`~crewlet.learning.persist_decider.PersistDecider` and the
``reflect_and_persist`` builtin) in every Plan-phase prompt --
filtered for relevance to the current task by an aux-LLM call.

This is the **read side** of the agent-scope memory model.  The
write side (PersistDecider) classifies into NOOP / DOC / LONG /
SHORT and writes LONG/SHORT here at agent scope with metadata
``kind=memory_long`` or ``kind=memory_short``.  SHORT entries
carry ``ttl_until`` and are filtered out at read time once expired.

Design highlights:

- **Aux-LLM relevance filter**, not pure embedding similarity.  The
  LLM understands "rule X applies to topic Y" relationships that
  cosine similarity often misses ("semantic commits" rule applied
  to a "fix search-indexing PR" task).  Cost is bounded for
  realistic memory pools (<200 entries per agent).
- **Sender-aware filter prompt.**  When the trigger has an
  identifiable counterparty, the filter is told who the current
  sender is in a structured ``Current sender:`` line so it can
  reject memories naming a *different* subject (e.g. a
  "Sam prefers …" preference must not apply when the sender is
  Miles).  Without this, the filter has only Slack user IDs in the
  task body to reason from.
- **Show-nothing on filter failure / unavailability.**  When the aux
  LLM isn't wired, its call fails, or it returns unparseable /
  empty output, the digest renders nothing.  There is deliberately
  no recency-only fallback: it would silently bypass the filter and
  leak per-subject memories on cap hits or transient outages --
  preferring "no memory this turn" over "the wrong memory" is the
  safer default for a multi-party agent.
- **Resolved-selection annotation.**  The aux LLM returns indices
  (e.g. ``[0, 3]``) into the candidate list it was given.  When the
  framework publishes the aux phase event it appends a
  ``→ Resolved selection:`` block listing the actual memory text
  for each picked index, so the dashboard view shows what the
  indices map to without forcing operators to scroll up to the
  candidate list.
- **Char-budgeted output** so the digest never blows the prompt.
"""

from __future__ import annotations

import json
import re
from datetime import datetime
from typing import Any

from crewlet._logging import get_logger
from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.learning._aux_telemetry import (
    complete_with_telemetry,
    format_response_with_reasoning,
)
from crewlet.learning.diary import DIARY_KINDS, AgentDiary
from crewlet.learning.diary import KIND_LONG as DIARY_LONG
from crewlet.learning.diary import KIND_SHORT as DIARY_SHORT
from crewlet.learning.interaction import (
    InboundInteraction,
    merge_interactions_by_sender,
)
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import Completion, LLMProvider, Message

logger = get_logger("learning.personal_memory")

# Re-exported under ``KIND_LONG`` / ``KIND_SHORT`` so call sites can
# import the kind constants from a single module without depending on
# the underlying diary module's naming.
KIND_LONG = DIARY_LONG
KIND_SHORT = DIARY_SHORT
_MEMORY_KINDS = DIARY_KINDS

# Tunable defaults; callers can override via the fetcher's args.
DEFAULT_CHAR_BUDGET = 1500
DEFAULT_CANDIDATE_POOL_LIMIT = 100
# Hybrid candidate selection for the Plan-phase prefetch: vector top-K
# (semantic match against the trigger) is unioned with recency top-K
# (broadly-applicable rules that may not topical-match), deduped, then
# capped at ``DEFAULT_CANDIDATE_POOL_LIMIT`` before the aux filter sees
# the merged pool.  50 + 50 leaves room for the cap to bite only on
# heavily-overlapping pools; in typical-size diaries the union is well
# under 100.
DEFAULT_VECTOR_LIMIT = 50
DEFAULT_RECENCY_LIMIT = 50
# Headroom for thinking-capable models.  The filter's visible output is
# tiny (a JSON array of <=8 indices, ~10 tokens), but extended-thinking
# models can spend a few thousand tokens reasoning before they emit
# anything visible.  A tight cap (e.g. 200) makes thinking models hit
# the cap with empty content -- the call returns
# ``output_tokens == max_tokens`` and ``content == ""`` -- so every
# filtered turn would degrade to the show-nothing path.
DEFAULT_FILTER_OUTPUT_TOKENS = 2000
DEFAULT_MAX_SELECTED = 8

# Rendered when the ``## Personal memory`` block would otherwise go
# silently empty despite the agent HAVING memory rows -- either the
# thin-trigger gate skipped the aux filter, or the filter ran and
# returned ``[]``.  The block becomes this hint instead: a
# context-thin trigger ("yes", "+1") and a fresh agent both render the
# same empty block otherwise, leaving the planner with no signal that
# re-filtering would help.
EMPTY_FILTER_HINT = (
    "(no stored memories surfaced at turn start -- re-run the "
    "filter via ``refresh_memory`` after you've gathered more "
    "context about the conversation)"
)


_FILTER_SYSTEM_PROMPT = """You are a memory-relevance filter for an AI agent.

Given the agent's current task and a numbered list of stored personal
memories, return the indices of memories genuinely relevant to this
task.

Output format (strict):
- A single JSON array of integer indices in priority order.
- Most relevant first.  No prose before or after.
- Examples: [3, 0, 7]   or   []
- At most 8 indices.

The user prompt may include a ``Current sender:`` line identifying
who triggered this turn.  Use it when judging per-subject relevance
(see rule 3 below) -- but it is NOT a hard filter on its own; an
operational rule that mentions a different subject can still apply.

A memory is RELEVANT if ANY ONE of the following holds.  Evaluate
all three before deciding to exclude.

The key distinction underlying these rules is what each memory is
*about*.  An operational memory is about a situation (a state, a
condition, a workflow rule) -- it tells you what to do.  A
per-subject memory is about how to engage with a named person -- it
tells you how to interact with them.  The subject-match filter
applies to the second kind only; operational memories may mention a
person incidentally (the OOO subject, the routing target) but the
rule's meaning is about the situation, not about that person.

1. **Operational context bearing on the task topic.**  Anything
   describing the *state of the world* that affects how to handle
   this kind of task right now -- routing / coverage / OOO state,
   freeze windows, incident-response mode, deployment locks,
   capacity constraints.  May name people; the rule still applies
   regardless of who's asking.  Subject mismatch alone is NOT
   grounds for exclusion here.

2. **General rules, conventions, or preferences for actions the
   task requires.**  Workflow-wide patterns ("use semantic commit
   messages", "review every backend PR before merge").  No subject
   filter -- they apply to every instance of the task type.

3. **Per-subject preferences for how to engage with someone the task
   explicitly involves.**  Memory describes *how to interact with* a
   named person who IS the current sender, OR is the recipient, OR
   is mentioned-by-name in the task body.  Subject mismatch IS a
   hard filter for this rule -- a preference about someone not party
   to the task does not apply.

EXCLUDE only when none of the three above apply -- a per-subject
preference about someone not party to the task, or a memory
describing a resolved past state that no longer holds.

If nothing applies, return [].
"""


async def fetch_personal_memory_block(
    *,
    diary: AgentDiary | None,
    agent_id: str,
    role: Role | None,
    task_description: str,
    interactions: list[InboundInteraction] | None = None,
    llm_providers: dict[str, LLMProvider] | None = None,
    event_queue: Any = None,
    turn_id: str = "",
    char_budget: int = DEFAULT_CHAR_BUDGET,
    candidate_pool_limit: int = DEFAULT_CANDIDATE_POOL_LIMIT,
    trigger_requires_recon: bool = False,
) -> str:
    """Render the agent's personal-memory digest for the Plan prompt.

    Returns an empty string when:
    - diary / agent_id missing,
    - no memory rows match,
    - the aux LLM filter is unavailable (``role`` / ``llm_providers``
      missing, no ``llm_auxiliary`` resolves),
    - the aux LLM call fails / returns empty / returns unparseable
      output,
    - the filter explicitly judged nothing relevant,
    - all selected rows render to nothing within the char budget.

    There is **no recency-sorted fallback**.  A permissive "show
    everything within budget" path would silently bypass the filter
    on cap hits / outages and leak per-subject memories ("User Sam
    prefers …" surfaced for a turn from someone other than Sam).
    The contract: if the filter doesn't approve a memory, the
    planner doesn't see it.  Operators must wire ``llm_auxiliary``
    (or rely on ``role.llm`` resolution) to get any memory at all.

    ``interactions`` carries the trigger-side counterparties so the
    filter prompt can include a structured ``Current sender:`` line
    (one per distinct sender on a coalesced trigger) -- without it,
    the filter has only Slack/Jira IDs in the task body to reason
    about subject identity.

    ``trigger_requires_recon`` short-circuits the aux-LLM call: when
    the trigger is a bare pointer (a webhook naming a
    thing-that-changed), running the filter against it is
    near-guaranteed low-value -- it has no real context to match
    memories against.  Rather than burn an aux call to produce ``[]``,
    skip straight to the ``EMPTY_FILTER_HINT`` nudge (when the agent
    has memory rows) so the planner re-runs ``refresh_memory`` after
    recon.  The diary list is still fetched -- it is a cheap DB read,
    and we need it to know whether to nudge at all.

    The aux-LLM call runs in parallel with the other plan-phase
    prefetches (counterparty, episodes, skills, onboarding hint) via
    ``asyncio.gather`` -- wall-clock impact is bounded by the slowest
    prefetch, not the sum.
    """
    task = (task_description or "").strip()
    if not task:
        return ""

    candidates = await fetch_existing_memories(
        diary=diary,
        agent_id=agent_id,
        query=task,
        candidate_pool_limit=candidate_pool_limit,
    )
    if not candidates:
        return ""

    if trigger_requires_recon:
        # Thin trigger: the bare pointer has nothing for the filter to
        # match against.  Skip the aux call; nudge the planner to
        # re-filter via ``refresh_memory`` once it has done recon.
        logger.debug(
            "personal_memory_filter_skipped_thin_trigger",
            agent_id=agent_id,
            turn_id=turn_id,
            candidate_count=len(candidates),
        )
        return EMPTY_FILTER_HINT

    if role is None or not llm_providers:
        # Filter unavailable.  Refuse to surface anything rather than
        # falling back to recency -- see module docstring for the
        # per-subject leak rationale.
        logger.debug(
            "personal_memory_filter_unavailable",
            agent_id=agent_id,
            turn_id=turn_id,
            candidate_count=len(candidates),
        )
        return ""

    indices = await _select_via_aux_llm(
        task=task,
        candidates=candidates,
        role=role,
        llm_providers=llm_providers,
        interactions=interactions,
        event_queue=event_queue,
        agent_id=agent_id,
        turn_id=turn_id,
    )
    if indices is None:
        # Filter ran but produced no usable result.  Already logged at
        # the failure site (empty content, exception, parse failure);
        # render nothing.
        return ""
    if not indices:
        # Filter intentionally selected nothing -- but the agent has
        # candidates in its memory pool.  Render a one-line nudge so
        # the planner knows there ARE memories, just none that the
        # turn-start filter pass surfaced.  The consolidated
        # retrieval-research guidance (the ``refresh_memory`` line,
        # gated on that tool being registered) tells the planner what
        # to do about it -- see ``RETRIEVAL_RESEARCH_GUIDANCE`` in
        # ``agent/prompts.py``.
        # Without this nudge a fresh agent and a context-thin trigger
        # (e.g. "yes") look identical in the prompt and the planner has
        # no signal to refresh.
        return EMPTY_FILTER_HINT
    rendered = _render_selected(candidates, indices, char_budget)
    if rendered and diary is not None:
        # Retrieval telemetry: bump retrieval_count + last_retrieved_at on
        # each row the filter actually surfaced to the prompt.  One
        # batched UPDATE -- this runs on the Plan-phase critical path,
        # so a per-row await chain would be N serial round-trips.
        # Best-effort -- bookkeeping never breaks the digest.
        retrieved_ids = [
            candidates[idx].get("id") or ""
            for idx in indices
            if 0 <= idx < len(candidates) and candidates[idx].get("id")
        ]
        await diary.mark_retrieved_many(retrieved_ids)
    return rendered


async def fetch_existing_memories(
    *,
    diary: AgentDiary | None,
    agent_id: str,
    query: str = "",
    candidate_pool_limit: int = DEFAULT_CANDIDATE_POOL_LIMIT,
    vector_limit: int = DEFAULT_VECTOR_LIMIT,
    recency_limit: int = DEFAULT_RECENCY_LIMIT,
) -> list[dict[str, Any]]:
    """Return the agent's currently-effective LONG/SHORT memory rows.

    Two call shapes:

    * **With ``query``** -- hybrid candidate selection.  The union of
      :meth:`AgentDiary.search_for_agent` top-K (vector similarity
      against the query; catches topical matches) and
      :meth:`AgentDiary.list_for_agent` top-K (recency; catches
      broadly-applicable rules that may not topical-match), deduped
      by row id, capped at ``candidate_pool_limit``.  Used by
      :func:`fetch_personal_memory_block` so old-but-relevant
      memories can reach the aux filter even when they fall outside
      the recency window of a long-lived agent's diary.

    * **Without ``query``** -- pure recency (the historical
      behaviour).  Used by
      :class:`~crewlet.learning.persist_decider.PersistDecider`'s
      write-side dedup: the question there is "is this fact already
      known?", not "is this fact semantically related to anything?",
      so recency-50 is the right candidate set.

    The hybrid path falls back to pure recency when the diary has
    no embedding provider wired (test / in-memory mode) or when the
    vector search fails -- the recency half is always available.

    Empty list on missing inputs or fetch failure -- callers treat
    "no memories" and "fetch broken" identically.
    """
    if diary is None or not agent_id:
        return []
    vector_hits: list[dict[str, Any]] = []
    if query:
        try:
            vector_hits = await diary.search_for_agent(
                agent_id,
                query=query,
                limit=vector_limit,
                include_expired=False,
            )
        except Exception:
            logger.exception("personal_memory_search_failed", agent_id=agent_id)
            vector_hits = []
    # ``AgentDiary.list_for_agent`` already swallows DB failures and
    # returns ``[]``, drops TTL-expired rows, and trims to the pool
    # size; ``search_for_agent`` does the same for its half.
    recent = await diary.list_for_agent(
        agent_id, limit=recency_limit, include_expired=False
    )
    # Dedup by id with vector hits first (high semantic relevance),
    # then recency rows not already in the vector pool.  Cap at
    # ``candidate_pool_limit``.
    seen: set[str] = set()
    merged: list[dict[str, Any]] = []
    for row in (*vector_hits, *recent):
        rid = str(row.get("id") or "")
        if not rid or rid in seen:
            continue
        seen.add(rid)
        merged.append(row)
        if len(merged) >= candidate_pool_limit:
            break
    return _select_memory_candidates(merged, candidate_pool_limit=candidate_pool_limit)


# ---------------------------------------------------------------------
# Internals
# ---------------------------------------------------------------------


def _select_memory_candidates(
    docs: list[dict[str, Any]],
    *,
    candidate_pool_limit: int,
) -> list[dict[str, Any]]:
    """Trim diary docs to the candidate pool, dropping empty-content rows.

    ``AgentDiary.list_for_agent`` is the upstream filter for kind +
    TTL; this helper just enforces the candidate-pool cap and the
    "no empty rows in the prompt" rule.  Docs arrive in
    ``created_at DESC`` order from the diary, so the head of the
    list is the most recent.
    """
    out: list[dict[str, Any]] = []
    for doc in docs:
        content = (doc.get("content") or "").strip()
        if not content:
            continue
        meta = doc.get("metadata") or {}
        if isinstance(meta, dict) and meta.get("kind") not in _MEMORY_KINDS:
            # Defensive: the diary's CHECK constraint should already
            # have rejected anything outside DIARY_KINDS, but a
            # row that snuck in via a manual write is silently
            # dropped here rather than poisoning the prompt.
            continue
        out.append(doc)
        if len(out) >= candidate_pool_limit:
            break
    return out


async def _select_via_aux_llm(
    *,
    task: str,
    candidates: list[dict[str, Any]],
    role: Role,
    llm_providers: dict[str, LLMProvider],
    interactions: list[InboundInteraction] | None,
    event_queue: Any,
    agent_id: str,
    turn_id: str,
) -> list[int] | None:
    """Aux-LLM relevance filter.  Returns selected indices, or
    ``None`` when the filter could not produce a usable result
    (no aux provider, LLM call failed, empty response, parse error).

    Empty response is logged at WARNING -- it is the symptom of a
    cap hit on a thinking-capable model and should be visible in
    operator traces so ``DEFAULT_FILTER_OUTPUT_TOKENS`` can be tuned.
    """
    try:
        provider_key, provider = resolve_phase_provider(
            role, "auxiliary", llm_providers
        )
    except Exception:
        logger.debug("personal_memory_no_aux_provider", agent_id=agent_id)
        return None
    user_prompt = _build_filter_user_prompt(task, candidates, interactions=interactions)

    def _format(comp: Completion) -> str:
        # Stamp the dashboard event with the LLM's raw response (with
        # reasoning wrapped in <think>) PLUS a "→ Resolved selection:"
        # block listing the actual memory text for each picked index.
        # The candidate-list lookup happens here in the closure where
        # we still have the candidates in scope; the underlying
        # completion returned to the caller is unchanged.
        return _format_aux_response_with_resolution(comp, candidates)

    try:
        completion = await complete_with_telemetry(
            provider=provider,
            messages=[
                Message(role="system", content=_FILTER_SYSTEM_PROMPT),
                Message(role="user", content=user_prompt),
            ],
            worker="memory_filter",
            role_name=role.name,
            provider_key=provider_key,
            event_queue=event_queue,
            agent_id=agent_id,
            turn_id=turn_id,
            response_formatter=_format,
            temperature=0.2,
            max_tokens=DEFAULT_FILTER_OUTPUT_TOKENS,
        )
    except Exception:
        logger.exception("personal_memory_filter_llm_failed", agent_id=agent_id)
        return None
    text = (completion.content or "").strip()
    if not text:
        # Most often this is a thinking-capable model burning the
        # output cap on internal reasoning before it can emit the
        # JSON array.  Surface it loudly so operators bump
        # ``DEFAULT_FILTER_OUTPUT_TOKENS`` (or wire a non-thinking
        # ``llm_auxiliary``) instead of silently losing the filter.
        logger.warning(
            "personal_memory_filter_empty_response",
            agent_id=agent_id,
            turn_id=turn_id,
            output_tokens=getattr(completion, "output_tokens", 0),
            max_tokens_cap=DEFAULT_FILTER_OUTPUT_TOKENS,
            finish_reason=getattr(completion, "finish_reason", ""),
        )
        return None
    indices = _parse_indices(text)
    # Distinguish "[]" (intentional empty selection -> honour it,
    # render nothing) from unparseable garbage (-> failure, also
    # render nothing but logged separately).
    if not indices and not _looks_like_empty_array(text):
        logger.warning(
            "personal_memory_filter_unparseable_response",
            agent_id=agent_id,
            turn_id=turn_id,
            response_preview=text[:200],
        )
        return None
    return indices


def _format_aux_response_with_resolution(
    completion: Completion,
    candidates: list[dict[str, Any]],
) -> str:
    """Decorate the aux filter's event response with resolved selections.

    Default :func:`format_response_with_reasoning` produces something
    like ``<think>...</think>\\n\\n[0, 3]``.  That tells the operator
    *which* indices were picked but not *what* they map to without
    scrolling up to the candidate list in the prompt.  Append a
    ``→ Resolved selection:`` block listing each picked memory's
    content so the dashboard view stands on its own.

    Quietly skips out-of-bounds indices and returns the base format
    unchanged when nothing was selected (an empty ``[]`` is
    self-explanatory; no need to render an empty resolution block).
    """
    base = format_response_with_reasoning(completion)
    text = (completion.content or "").strip()
    if not text:
        return base
    indices = _parse_indices(text)
    # ``_parse_indices`` returns ``[]`` for both "[] in source" and
    # "garbage we couldn't parse" -- in either case there's no
    # resolution to render, so just return the base format.
    if not indices:
        return base
    bullets: list[str] = []
    for i in indices:
        if not (0 <= i < len(candidates)):
            continue
        content = (candidates[i].get("content") or "").strip().replace("\n", " ")
        if not content:
            continue
        if len(content) > 240:
            content = content[:240] + "..."
        bullets.append(f"  {i}. {content}")
    if not bullets:
        return base
    return base + "\n\n→ Resolved selection:\n" + "\n".join(bullets)


def _looks_like_empty_array(text: str) -> bool:
    """Return True when the text contains a recognisable empty JSON
    array.  Used to tell "LLM intentionally said nothing relevant"
    from "couldn't parse anything"."""
    stripped = text.strip()
    if stripped == "[]":
        return True
    return bool(re.search(r"\[\s*\]", stripped))


def _build_filter_user_prompt(
    task: str,
    candidates: list[dict[str, Any]],
    *,
    interactions: list[InboundInteraction] | None = None,
) -> str:
    lines: list[str] = []
    sender_lines = _render_current_sender_lines(interactions)
    if sender_lines:
        lines.extend(sender_lines)
        lines.append("")
    lines.extend(
        [
            "Task the agent is about to plan:",
            f'"{task[:1200]}"',
            "",
            "Stored memories (numbered):",
        ]
    )
    for i, doc in enumerate(candidates):
        content = (doc.get("content") or "").strip().replace("\n", " ")
        if len(content) > 200:
            content = content[:200] + "..."
        lines.append(f"{i}. {content}")
    lines.append("")
    lines.append("Output (JSON array of indices, priority order):")
    return "\n".join(lines)


def _render_current_sender_lines(
    interactions: list[InboundInteraction] | None,
) -> list[str]:
    """Return ``Current sender: <label> (platform:external_id)`` lines.

    One line per distinct identifiable sender on the trigger — a
    coalesced multi-human thread surfaces each participant so the
    per-subject relevance rules can fire for all of them.  Empty when
    the trigger has no identifiable sender (internal ``TaskAssigned``
    and friends) -- the filter then has to reason about subjects from
    the task body alone, same as before.
    """
    lines: list[str] = []
    for interaction in merge_interactions_by_sender(interactions):
        described = interaction.sender.describe()
        if described:
            lines.append(f"Current sender: {described}")
    return lines


def _parse_indices(text: str) -> list[int]:
    """Extract integer indices from the aux-LLM's JSON-array output."""
    text = text.strip()
    parsed: Any
    try:
        parsed = json.loads(text)
    except Exception:
        match = re.search(r"\[[\d,\s]*\]", text)
        if not match:
            return []
        try:
            parsed = json.loads(match.group())
        except Exception:
            return []
    if not isinstance(parsed, list):
        return []
    out: list[int] = []
    seen: set[int] = set()
    for v in parsed:
        try:
            idx = int(v)
        except (TypeError, ValueError):
            continue
        if idx in seen:
            continue
        seen.add(idx)
        out.append(idx)
    return out[:DEFAULT_MAX_SELECTED]


def _render_selected(
    candidates: list[dict[str, Any]],
    indices: list[int],
    char_budget: int,
) -> str:
    bullets: list[str] = []
    used = 0
    for idx in indices:
        if not (0 <= idx < len(candidates)):
            continue
        rendered = _render_bullet(candidates[idx])
        if not rendered:
            continue
        if used + len(rendered) > char_budget:
            break
        bullets.append(rendered)
        used += len(rendered) + 1
    return "\n".join(bullets)


def _render_bullet(doc: dict[str, Any]) -> str:
    content = (doc.get("content") or "").strip()
    if not content:
        return ""
    meta = doc.get("metadata") or {}
    expiry = meta.get("ttl_until") if isinstance(meta, dict) else None
    if expiry:
        # Render SHORT entries with their expiry so the LLM knows the
        # freshness window.  Format as ISO date only (drop time).
        try:
            d = datetime.fromisoformat(str(expiry))
            tag = f" (until {d.date().isoformat()})"
        except ValueError:
            tag = ""
        return f"-{tag} {content}"
    return f"- {content}"
