"""Relevant-knowledge digest for the Plan-phase prompt.

Surfaces team-published knowledge-base documents -- playbooks,
runbooks, ADRs, conventions, design docs -- relevant to the current
task, as a ``## Relevant knowledge`` block in the Plan prompt.

How it works: one auxiliary-LLM call turns the turn's trigger context
into a short text-search query; the engine's
:class:`~crewlet.knowledge.protocol.KnowledgeSearcher` runs that query
live against the team knowledge base (Confluence CQL or Plane pages,
scoped to the org's accessible containers); the top hits are rendered
as title + snippet bullets.

There is no local index and no embedding step -- the knowledge base is
queried directly at turn time, so the block always reflects current
content.  This mirrors the project's stance that procedural knowledge
lives in the team knowledge base; the engine just searches it on the
agent's behalf so the planner sees relevant docs without having to
think to call a search tool itself.

Design highlights:

- **Aux-generated query.**  The LLM is a far better search-query
  writer than raw trigger text -- it distils keywords, preserves
  identifiers, and drops conversational filler.  One call per turn.

- **Trust the backend's ordering.**  Hits come back ordered by the
  backend (Confluence ranks by CQL relevance; Plane orders by
  recency); the prefetch renders the top N within a char budget
  rather than running a second relevance-filter pass.

- **Auto-drafts gated.**  Pages under the ``Auto-Drafted Skills``
  parent are excluded -- those are unreviewed LLM-proposed drafts.

- **Show-nothing on failure / unavailability.**  No aux provider, an
  empty query, or a search error renders nothing rather than noise.

- **Char-budgeted output** so the digest never blows the prompt.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from crewlet._logging import get_logger
from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.knowledge.protocol import AUTO_DRAFTED_PARENT
from crewlet.learning._aux_telemetry import complete_with_telemetry
from crewlet.org.models import Organization, Role
from crewlet.providers.llm.protocol import LLMProvider, Message

if TYPE_CHECKING:
    from crewlet.knowledge.protocol import KnowledgeHit, KnowledgeSearcher

logger = get_logger("learning.relevant_knowledge")

# Tunable defaults; callers can override via the fetcher's args.
DEFAULT_CHAR_BUDGET = 1500
DEFAULT_RESULT_LIMIT = 6
# Headroom for the query-generation aux call.  The visible output is
# one short line, but extended-thinking models can spend a few hundred
# tokens reasoning first; a cap hit yields empty content, which the
# caller degrades to an empty block.
DEFAULT_QUERY_OUTPUT_TOKENS = 1000

# Rendered when the ``## Relevant knowledge`` block would otherwise go
# silently empty -- the thin-trigger gate skipped the search, or the
# search ran and found nothing.  Framing mirrors
# ``personal_memory.EMPTY_FILTER_HINT``: re-search after recon is the
# expected pattern, not an unusual escape hatch.
EMPTY_FILTER_HINT = (
    "(no team documents surfaced at turn start -- search your team "
    "knowledge base with a focused query once you have "
    "gathered more context about what the task actually needs)"
)


_QUERY_SYSTEM_PROMPT = """You turn an AI agent's current task into a \
search query for the team's knowledge base.

Output format (strict):
- A single line of 2-8 search keywords or key phrases.
- No prose, no explanation, no quotes, no JSON -- just the query text.
- Preserve exact identifiers verbatim (error codes, ticket keys,
  API / function / service names).
- Focus on the procedural or reference material the agent would look
  up: runbook topics, conventions, domain terms, methodologies.
- Drop conversational filler and people's names unless a person is
  themselves the subject of the lookup.

Examples:
- Task: "investigate the latency spike on checkout after the Tuesday
  deploy" -> latency spike checkout deployment rollback incident response
- Task: "review PR-123 adding the rate limiter"
  -> code review checklist rate limiting conventions
"""


async def fetch_relevant_knowledge_block(
    *,
    searcher: KnowledgeSearcher | None,
    role: Role | None,
    org: Organization | None,
    task_description: str,
    llm_providers: dict[str, LLMProvider] | None = None,
    event_queue: Any = None,
    agent_id: str = "",
    turn_id: str = "",
    char_budget: int = DEFAULT_CHAR_BUDGET,
    exclude_ancestors: list[str] | None = None,
    trigger_requires_recon: bool = False,
) -> str:
    """Render the relevant-knowledge digest for the Plan prompt.

    Returns an empty string when:

    - ``searcher`` / ``role`` / ``org`` missing, or the task is empty,
    - ``searcher.can_search`` says a search is a guaranteed no-op (no
      configured scope and no per-agent backend credentials),
    - the aux LLM is unavailable (``llm_providers`` missing, no
      ``llm_auxiliary`` resolves) or its query-generation call fails /
      returns empty,
    - the search returns nothing renderable within the char budget.

    Returns :data:`EMPTY_FILTER_HINT` when the thin-trigger gate
    skipped the search, or the search ran and found nothing -- the
    planner then knows an explicit knowledge-base search would help.

    ``exclude_ancestors`` defaults to ``[AUTO_DRAFTED_PARENT]`` so
    unpublished auto-drafts stay hidden; pass ``[]`` to disable.

    ``trigger_requires_recon`` short-circuits the aux call + search:
    when the trigger is a bare pointer (a webhook naming a
    thing-that-changed) there is nothing worth searching on yet, so
    skip straight to the nudge.

    The call runs in parallel with the other Plan-phase prefetches via
    ``asyncio.gather`` -- wall-clock impact is bounded by the slowest
    prefetch, not the sum.
    """
    task = (task_description or "").strip()
    if not task:
        return ""
    if searcher is None or role is None or org is None:
        return ""

    # The searcher owns the scope vocabulary (spaces vs projects) and
    # the unscoped-vs-nothing rule; ``can_search`` is its cheap pre-gate
    # so a guaranteed-empty search never spends the aux-LLM call.
    if not searcher.can_search(role=role, org=org):
        return ""

    if trigger_requires_recon:
        logger.debug(
            "relevant_knowledge_skipped_thin_trigger",
            agent_id=agent_id,
            turn_id=turn_id,
        )
        return EMPTY_FILTER_HINT

    if not llm_providers:
        logger.debug(
            "relevant_knowledge_aux_unavailable",
            agent_id=agent_id,
            turn_id=turn_id,
        )
        return ""

    query = await _generate_search_query(
        task=task,
        role=role,
        llm_providers=llm_providers,
        event_queue=event_queue,
        agent_id=agent_id,
        turn_id=turn_id,
    )
    if not query:
        return ""

    excludes = (
        list(exclude_ancestors)
        if exclude_ancestors is not None
        else [AUTO_DRAFTED_PARENT]
    )
    try:
        results = await searcher.search(
            query=query,
            role=role,
            org=org,
            limit=DEFAULT_RESULT_LIMIT,
            exclude_ancestors=excludes,
        )
    except Exception:
        logger.exception(
            "relevant_knowledge_search_failed",
            agent_id=agent_id,
            turn_id=turn_id,
        )
        return ""
    if not results:
        return EMPTY_FILTER_HINT
    return _render_results(results, char_budget)


# ---------------------------------------------------------------------
# Internals
# ---------------------------------------------------------------------


async def _generate_search_query(
    *,
    task: str,
    role: Role,
    llm_providers: dict[str, LLMProvider],
    event_queue: Any,
    agent_id: str,
    turn_id: str,
) -> str:
    """Aux-LLM call: turn the task into a knowledge-base text query.

    Returns the query string, or ``""`` when no aux provider resolves,
    the call fails, or the response is empty.
    """
    try:
        provider_key, provider = resolve_phase_provider(
            role, "auxiliary", llm_providers
        )
    except Exception:
        logger.debug("relevant_knowledge_no_aux_provider", agent_id=agent_id)
        return ""

    try:
        completion = await complete_with_telemetry(
            provider=provider,
            messages=[
                Message(role="system", content=_QUERY_SYSTEM_PROMPT),
                Message(role="user", content=_build_query_user_prompt(task)),
            ],
            worker="relevant_knowledge_query",
            role_name=role.name,
            provider_key=provider_key,
            event_queue=event_queue,
            agent_id=agent_id,
            turn_id=turn_id,
            temperature=0.2,
            max_tokens=DEFAULT_QUERY_OUTPUT_TOKENS,
        )
    except Exception:
        logger.exception("relevant_knowledge_query_llm_failed", agent_id=agent_id)
        return ""

    text = (completion.content or "").strip()
    if not text:
        logger.warning(
            "relevant_knowledge_query_empty_response",
            agent_id=agent_id,
            turn_id=turn_id,
            output_tokens=getattr(completion, "output_tokens", 0),
            max_tokens_cap=DEFAULT_QUERY_OUTPUT_TOKENS,
            finish_reason=getattr(completion, "finish_reason", ""),
        )
        return ""
    # The model may emit stray newlines; take the first real line and
    # drop any surrounding quotes a chatty model wrapped it in.
    first_line = next((ln.strip() for ln in text.splitlines() if ln.strip()), "")
    return first_line.strip("\"'")


def _build_query_user_prompt(task: str) -> str:
    return (
        f'Task the agent is about to plan:\n"{task[:1200]}"\n\n'
        "Knowledge-base search query:"
    )


def _render_results(results: list[KnowledgeHit], char_budget: int) -> str:
    """Render search hits as Markdown bullets within the char budget."""
    bullets: list[str] = []
    used = 0
    for hit in results:
        rendered = _render_bullet(hit)
        if not rendered:
            continue
        if used + len(rendered) > char_budget:
            break
        bullets.append(rendered)
        used += len(rendered) + 1
    if not bullets:
        return ""
    bullets.append(
        "To read any page in full, look it up by title with your "
        "knowledge-base search / get-page tools."
    )
    return "\n".join(bullets)


def _render_bullet(hit: KnowledgeHit) -> str:
    """Render one search hit as ``- **<title>**: <snippet>``.

    Falls back to the title alone when the page has no snippet; empty
    string when the hit has neither -- a degenerate row the caller
    skips.
    """
    title = (hit.title or "").strip()
    snippet = (hit.snippet or "").strip()
    if not title and not snippet:
        return ""
    if not title:
        title = "(untitled)"
    if snippet:
        return f"- **{title}**: {snippet}"
    return f"- **{title}**"
