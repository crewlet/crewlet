"""Agent-learning builtins: ``query_episodes``, ``reflect_and_persist``,
``refine_skill``, ``refresh_memory``.

All four tools are registered from :meth:`crewlet.engine.Engine.start`
once their upstream dependencies (Postgres pool, embeddings, knowledge
system) are available.

``query_episodes`` is Plan-phase-only and returns a list of
similar past turns.  When the role has ``llm_auxiliary`` configured
and ``learning.reflect.summarize_episodes`` is true, raw hits are
passed through a cheap model for a compact rendering before being
returned — the planner sees bullets, not JSON.

``reflect_and_persist`` is the
LLM-facing surface for in-flight personal-memory writes.  It mirrors
PersistDecider's LONG / SHORT tiers (default LONG; pass ``ttl_days``
for SHORT) and writes at agent scope only.  The stamped metadata
matches PersistDecider's writes (``kind=memory_long`` /
``memory_short``, ``source=reflect_and_persist``) so both flow
through the same personal-memory digest at read time and ReflectEngine
can skip turns that already self-persisted.

``refresh_memory`` lets the planner re-run the
personal-memory relevance filter with context it has gathered during
the turn (e.g. a thread summary, ticket body, or counterparty
profile).  The Plan-phase pre-fetch runs the filter against the bare
trigger alone, which leaves two failure modes the LLM should fix
itself: (1) context-thin triggers ("yes", "+1") return ``[]`` even
when relevant memories exist; (2) richer triggers may return entries
that no longer look most-relevant once the real topic surfaces.  The
prompt guidance therefore tells the planner to refresh after any
meaningful recon -- not only when the initial block was empty.
Per-turn calls are capped (default 3) and idempotent on repeated
hints.
"""

from __future__ import annotations

import hashlib
import json
from collections import OrderedDict
from datetime import UTC, datetime, timedelta
from typing import Any

from crewlet._logging import get_logger
from crewlet.learning.diary import KIND_LONG as _KIND_LONG
from crewlet.learning.diary import KIND_SHORT as _KIND_SHORT
from crewlet.learning.diary import AgentDiary
from crewlet.learning.episode_store import EpisodeStoreProtocol
from crewlet.learning.interaction import salient_task_text
from crewlet.learning.personal_memory import fetch_personal_memory_block
from crewlet.learning.summarize import summarize_episodes
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import LLMProvider
from crewlet.tools.protocol import AgentContext, ToolResult
from crewlet.tools.registry import SimpleTool, ToolRegistry

logger = get_logger("learning.tools")

_VALID_OUTCOMES = {"done", "self_iterate", "failed"}

# Source marker on diary writes so downstream consumers (ReflectEngine,
# dashboards) can distinguish an in-turn self-persist from a post-turn
# deterministic reflection.
_SOURCE_REFLECT_AND_PERSIST = "reflect_and_persist"

# TTL cap in days for SHORT entries (mirrors PersistDecider).
_MAX_SHORT_TTL_DAYS = 180


def register_episode_tools(
    registry: ToolRegistry,
    episode_store: EpisodeStoreProtocol,
    *,
    llm_providers: dict[str, LLMProvider] | None = None,
    summarize: bool = True,
    summarize_max_tokens: int = 400,
    role_lookup: Any = None,
) -> None:
    """Register ``query_episodes`` with ``registry``.

    ``role_lookup`` is an optional callable ``(agent_handle) -> Role``
    used to resolve the caller's role for auxiliary-model routing.
    When omitted (or it returns ``None``), summarisation is skipped
    and the raw JSON payload is returned.
    """

    async def _query_episodes(
        params: dict[str, Any], context: AgentContext
    ) -> ToolResult:
        query = str(params.get("query", "")).strip()
        if not query:
            return ToolResult(success=False, error="query is required")
        if not context.agent_handle:
            return ToolResult(
                success=False,
                error="agent_handle not set on context — cannot scope query",
            )

        limit_raw = params.get("limit", 5)
        try:
            limit = max(1, min(int(limit_raw), 20))
        except (TypeError, ValueError):
            limit = 5

        outcome_filter = params.get("outcome_filter")
        if outcome_filter is not None:
            outcome_filter = str(outcome_filter).strip().lower() or None
            if outcome_filter is not None and outcome_filter not in _VALID_OUTCOMES:
                return ToolResult(
                    success=False,
                    error=(f"outcome_filter must be one of {sorted(_VALID_OUTCOMES)}"),
                )

        try:
            episodes = await episode_store.query(
                agent_handle=context.agent_handle,
                query_text=query,
                limit=limit,
                outcome_filter=outcome_filter,
            )
        except Exception as exc:
            logger.exception("query_episodes_failed", query=query)
            return ToolResult(success=False, error=f"query failed: {exc}")

        if not episodes:
            return ToolResult(success=True, output="(no prior episodes found)")

        raw = _format_episodes(episodes)
        if summarize and llm_providers and role_lookup is not None:
            role: Role | None = None
            try:
                role = role_lookup(context.agent_handle)
            except Exception:
                logger.exception("episode_role_lookup_failed")
            if role is not None:
                raw = await summarize_episodes(
                    raw_payload=raw,
                    role=role,
                    llm_providers=llm_providers,
                    max_tokens=summarize_max_tokens,
                    event_queue=getattr(context, "event_queue", None),
                    agent_id=context.agent_id,
                )

        return ToolResult(success=True, output=raw)

    registry.register(
        SimpleTool(
            name="query_episodes",
            description=(
                "Search your past completed turns by task/plan similarity. "
                "Use BEFORE planning a new task to find similar prior work "
                "and reuse the approach instead of deriving it again. "
                "Scoped to your own handle; you never see other agents' "
                "episodes. Optional outcome_filter narrows to one of "
                "'done' | 'self_iterate' | 'failed'."
            ),
            parameters={
                "type": "object",
                "properties": {
                    "query": {
                        "type": "string",
                        "description": "Natural-language description of the task.",
                    },
                    "limit": {
                        "type": "integer",
                        "description": "Max results to return (1-20, default 5).",
                        "minimum": 1,
                        "maximum": 20,
                    },
                    "outcome_filter": {
                        "type": "string",
                        "description": (
                            "Restrict to one review outcome. Omit to include "
                            "all outcomes."
                        ),
                        "enum": sorted(_VALID_OUTCOMES),
                    },
                },
                "required": ["query"],
            },
            fn=_query_episodes,
        )
    )
    logger.info("episode_tools_registered")


def register_reflect_and_persist_tool(
    registry: ToolRegistry,
    diary: AgentDiary,
) -> None:
    """Register the LLM-facing ``reflect_and_persist`` tool.

    Plan-phase-only -- enforced by leaving it out of the
    ``executor_always_on_tools`` list.  The tool writes a durable
    declarative fact to the agent's **personal diary**.  Mirrors the
    LONG / SHORT classification PersistDecider produces post-turn:
    when the LLM proposes ``ttl_days``, the write is treated as SHORT
    (with ``ttl_until`` set on the diary row); otherwise LONG.

    Both writers (this tool + PersistDecider) share the same
    :class:`~crewlet.learning.diary.AgentDiary` write surface so the
    rows flow through the same personal-memory digest at read time.

    Memory is **agent-scope only** -- consistent with the four-tier
    model (DOC for team-wide rules, LONG/SHORT for personal facts,
    NOOP for nothing).  Things the LLM thinks are team-shared should
    be handed off to a lead over the team's chat / issue tracker (a
    colleague-surface tool), not written here.
    """

    async def _reflect_and_persist(
        params: dict[str, Any], context: AgentContext
    ) -> ToolResult:
        content = str(params.get("content", "")).strip()
        if not content:
            return ToolResult(success=False, error="content is required")
        diary_ref: AgentDiary | None = getattr(context, "agent_diary", None) or diary
        if diary_ref is None:
            return ToolResult(success=False, error="agent diary unavailable")
        if not context.agent_id:
            return ToolResult(
                success=False,
                error="agent_id not set on context — cannot store memory",
            )

        # Optional ttl_days promotes the write from LONG to SHORT
        # (mirrors PersistDecider's SHORT tier).  Clamped to the same
        # band so a runaway value can't pin a row indefinitely.
        ttl_days_raw = params.get("ttl_days")
        ttl_days: int | None = None
        if ttl_days_raw is not None:
            try:
                ttl_days = max(1, min(int(ttl_days_raw), _MAX_SHORT_TTL_DAYS))
            except (TypeError, ValueError):
                return ToolResult(
                    success=False,
                    error="ttl_days must be a positive integer if provided",
                )
        kind = _KIND_SHORT if ttl_days is not None else _KIND_LONG
        ttl_until = (
            datetime.now(UTC) + timedelta(days=ttl_days)
            if ttl_days is not None
            else None
        )
        metadata: dict[str, Any] = {
            "agent_handle": context.agent_handle,
            "task_id": context.current_task_id,
        }
        note = str(params.get("note", "")).strip()
        if note:
            metadata["note"] = note

        try:
            doc_id = await diary_ref.write(
                agent_id=context.agent_id,
                kind=kind,
                content=content,
                ttl_until=ttl_until,
                source=_SOURCE_REFLECT_AND_PERSIST,
                turn_id="",
                metadata=metadata,
            )
        except Exception as exc:
            logger.exception("reflect_and_persist_failed")
            return ToolResult(success=False, error=f"persist failed: {exc}")

        ttl_str = ttl_until.isoformat() if ttl_until is not None else ""
        logger.info(
            "reflect_and_persist_stored",
            doc_id=doc_id,
            kind=kind,
            agent_handle=context.agent_handle,
            ttl_until=ttl_str,
        )
        ttl_msg = f", expires={ttl_str}" if ttl_str else ""
        return ToolResult(
            success=True,
            output=(f"Stored as personal memory ({kind}, doc_id={doc_id}{ttl_msg})."),
        )

    registry.register(
        SimpleTool(
            name="reflect_and_persist",
            description=(
                "Persist a durable fact to your personal memory. Write "
                "declarative facts, not instructions to yourself. "
                "'Stakeholder X prefers weekly digests' ✓ — "
                "'Always send weekly digests' ✗. Use for things worth "
                "carrying across turns; skip anything task-specific or "
                "ephemeral.  All writes are agent-scope (personal); "
                "things the team should follow belong in docs -- hand "
                "off to a lead over the team's chat or issue tracker "
                "instead.  "
                "Optionally pass ttl_days to mark a fact as ephemeral "
                "(operational context that ages out, e.g. 'Sarah is "
                "OOO until Friday')."
            ),
            parameters={
                "type": "object",
                "properties": {
                    "content": {
                        "type": "string",
                        "description": "Short declarative fact (1-2 sentences).",
                    },
                    "ttl_days": {
                        "type": "integer",
                        "minimum": 1,
                        "maximum": _MAX_SHORT_TTL_DAYS,
                        "description": (
                            "Optional. When set, the fact expires after "
                            "this many days (SHORT-tier memory).  Use for "
                            "ephemeral context like vacations or sprint "
                            "focus.  Omit for facts that should persist "
                            "indefinitely (LONG-tier memory)."
                        ),
                    },
                    "note": {
                        "type": "string",
                        "description": (
                            "Optional provenance note stored as metadata; "
                            "not part of the persisted content."
                        ),
                    },
                },
                "required": ["content"],
            },
            fn=_reflect_and_persist,
        )
    )
    logger.info("reflect_and_persist_tool_registered")


def _format_episodes(episodes: list[Any]) -> str:
    """Render episodes as a compact JSON payload for the LLM.

    Two physical row shapes share one payload list:

    - ``kind="raw"`` rows render as before -- per-turn detail
      (task, plan, tools, outcome).
    - ``kind="compacted"`` rows render with their aggregate fields
      (count, common pattern, success rate, subjects, exemplars) so
      the downstream summarizer / planner knows it's a summary
      across N original turns rather than a single occurrence.

    Mixing both kinds in the same payload is intentional: the
    similarity-search results returned by EpisodeStore.query may
    interleave them, and the summarizer prompt is robust to both
    formats.
    """
    payload: list[dict[str, Any]] = []
    for e in episodes:
        kind = getattr(e, "kind", "raw") or "raw"
        if kind == "compacted":
            payload.append(
                {
                    "kind": "compacted",
                    "count": getattr(e, "count", 1),
                    "common_task_pattern": getattr(e, "common_task_pattern", ""),
                    "common_outcome": getattr(e, "common_outcome", ""),
                    "success_rate": getattr(e, "success_rate", 0.0),
                    "subjects_involved": list(
                        getattr(e, "subjects_involved", []) or []
                    ),
                    "notable_patterns": getattr(e, "notable_patterns", ""),
                    "tool_sequence": list(e.tool_sequence),
                    "period_start": (e.started_at.isoformat() if e.started_at else ""),
                    "period_end": e.ended_at.isoformat() if e.ended_at else "",
                    "exemplar_turn_ids": [
                        str(x) for x in (getattr(e, "exemplar_turn_ids", []) or [])
                    ],
                }
            )
        else:
            payload.append(
                {
                    "kind": "raw",
                    "turn_id": str(e.turn_id),
                    "task_summary": e.task_summary,
                    "plan_summary": e.plan_summary,
                    "tool_sequence": list(e.tool_sequence),
                    "skills_used": list(e.skills_used),
                    "review_outcome": e.review_outcome,
                    "duration_ms": e.duration_ms,
                    "ended_at": e.ended_at.isoformat() if e.ended_at else "",
                }
            )
    return json.dumps({"episodes": payload}, indent=2)


def was_reflect_and_persist_called(tool_sequence: list[str]) -> bool:
    """True if a turn already called ``reflect_and_persist``.

    Used by the ``ReflectEngine`` / ``PersistDecider`` to avoid
    double-persisting when the LLM handled it in-turn.
    """
    return "reflect_and_persist" in tool_sequence


# ----------------------------------------------------------------------
# refine_skill
# ----------------------------------------------------------------------


_REFINE_ACTIONS = {"append", "replace_body"}


def register_refine_skill_tool(
    registry: ToolRegistry,
    store: Any,
    *,
    max_body_chars: int = 20000,
    max_versions_kept: int = 10,
) -> None:
    """Register the LLM-facing ``refine_skill`` tool.

    Plan-phase-only.  Patches one of the agent's own synthesized
    skills — either appends a bullet (``action="append"``) or replaces
    the entire body (``action="replace_body"``).  There is no other
    scope to consider: shared procedural artefacts live in the team
    knowledge base (Confluence or Plane), and editing those pages
    happens through the backend's own MCP tools, not this surface.
    """

    async def _refine_skill(
        params: dict[str, Any], context: AgentContext
    ) -> ToolResult:
        if store is None:
            return ToolResult(
                success=False, error="synthesized skill store unavailable"
            )
        if not context.agent_handle:
            return ToolResult(
                success=False,
                error="agent_handle not set on context — cannot refine",
            )
        name = str(params.get("name", "")).strip()
        if not name:
            return ToolResult(success=False, error="name is required")
        action = str(params.get("action", "append")).strip().lower()
        if action not in _REFINE_ACTIONS:
            return ToolResult(
                success=False,
                error=(
                    f"action must be one of {sorted(_REFINE_ACTIONS)} (got {action!r})"
                ),
            )
        patch = str(params.get("content", "")).strip()
        if not patch:
            return ToolResult(success=False, error="content is required")
        reason = str(params.get("reason", "")).strip()

        try:
            skill = await store.fetch(agent_handle=context.agent_handle, name=name)
        except Exception as exc:
            logger.exception("refine_skill_fetch_failed", name=name)
            return ToolResult(success=False, error=f"skill lookup failed: {exc}")
        if skill is None:
            return ToolResult(
                success=False,
                error=(
                    f"No synthesized skill named '{name}' owned by this "
                    "agent.  (You can only refine your own synthesized "
                    "skills.)"
                ),
            )

        if action == "append":
            new_body = (skill.content.rstrip() + "\n\n" + patch).strip()
        else:  # replace_body
            new_body = patch
        new_body = new_body[:max_body_chars]

        updated = skill.model_copy(update={"content": new_body})
        try:
            persisted = await store.update(
                updated,
                refinement_kind="refine_skill_tool",
                refinement_note=reason[:400],
                max_versions_kept=max_versions_kept,
            )
        except Exception as exc:
            logger.exception("refine_skill_update_failed", name=name)
            return ToolResult(success=False, error=f"update failed: {exc}")

        logger.info(
            "refine_skill_applied",
            agent_handle=context.agent_handle,
            name=name,
            action=action,
            version=persisted.version,
        )
        return ToolResult(
            success=True,
            output=(
                f"Refined '{name}' (action={action}, version={persisted.version})."
            ),
        )

    registry.register(
        SimpleTool(
            name="refine_skill",
            description=(
                "Patch one of your own synthesized skills when you "
                "notice it is outdated, incomplete, or wrong — don't "
                "wait for a reflection pass. `action=append` adds a "
                "bullet; `action=replace_body` rewrites the whole "
                "procedure. Only your agent-scope synthesized skills "
                "can be patched; unit-scope skills are read-only. "
                "Each refinement bumps the skill's version and "
                "archives the prior body for rollback."
            ),
            parameters={
                "type": "object",
                "properties": {
                    "name": {
                        "type": "string",
                        "description": "Skill name as it appears in use_skill.",
                    },
                    "action": {
                        "type": "string",
                        "enum": sorted(_REFINE_ACTIONS),
                        "description": "How to apply the patch.",
                    },
                    "content": {
                        "type": "string",
                        "description": (
                            "For `append`: the new bullet to add. "
                            "For `replace_body`: the full new body."
                        ),
                    },
                    "reason": {
                        "type": "string",
                        "description": (
                            "Optional short reason recorded in the "
                            "version history's refinement_note."
                        ),
                    },
                },
                "required": ["name", "content"],
            },
            fn=_refine_skill,
        )
    )
    logger.info("refine_skill_tool_registered")


# ----------------------------------------------------------------------
# refresh_memory
# ----------------------------------------------------------------------

# Bound on the in-memory per-turn refresh state map.  At
# ``DEFAULT_MAX_REFRESHES`` of 3 calls per turn this is far more than
# any sane traffic level, but the bound prevents the dict from growing
# unbounded across the lifetime of a long-running engine.
_MAX_REFRESH_TURNS_TRACKED = 1024

DEFAULT_MAX_REFRESHES = 3


class _RefreshTurnState:
    """Per-turn bookkeeping for the ``refresh_memory`` tool.

    ``count`` tracks distinct ``context_hint`` values processed (after
    dedup); ``cache`` serves the idempotency layer.  Repeat calls with
    the same hint return the cached output and do NOT consume cap.
    """

    __slots__ = ("count", "cache")

    def __init__(self) -> None:
        self.count = 0
        self.cache: dict[str, str] = {}


def _hint_hash(text: str) -> str:
    """Normalised SHA-1 of the hint for the idempotency cache key.

    Whitespace and case are normalised so a planner that re-issues
    the same hint with trivial differences still hits the cache.
    """
    normalised = " ".join(text.lower().split())
    return hashlib.sha1(normalised.encode("utf-8"), usedforsecurity=False).hexdigest()


def register_refresh_memory_tool(
    registry: ToolRegistry,
    diary: AgentDiary,
    *,
    llm_providers: dict[str, LLMProvider],
    role_lookup: Any,
    max_refreshes_per_turn: int = DEFAULT_MAX_REFRESHES,
) -> None:
    """Register the LLM-facing ``refresh_memory`` tool (Plan-phase only).

    The tool re-runs the personal-memory relevance filter with extra
    context the planner has gathered, returning the freshly-rendered
    digest as the tool output.  See module docstring for the gap this
    fills.

    ``role_lookup`` is a callable ``(agent_handle) -> Role | None`` --
    the engine wires it to the org's role registry.  Without a
    resolvable role the aux provider can't be picked and the tool
    returns an error pointing at the misconfiguration.

    Per-turn state (refresh count + idempotency cache) lives in the
    closure-bound ``_state_by_turn`` ``OrderedDict``, bounded by
    :data:`_MAX_REFRESH_TURNS_TRACKED` to keep memory finite across a
    long-running engine.  Per-turn scoping uses ``turn_context.turn_id``
    pulled off ``AgentContext.__dict__`` (the TurnEngine attaches the
    TurnContext there for tools that need it).
    """
    cap = max(1, int(max_refreshes_per_turn))
    state_by_turn: OrderedDict[str, _RefreshTurnState] = OrderedDict()

    def _state_for(turn_id: str) -> _RefreshTurnState:
        if turn_id in state_by_turn:
            state_by_turn.move_to_end(turn_id)
            return state_by_turn[turn_id]
        st = _RefreshTurnState()
        state_by_turn[turn_id] = st
        if len(state_by_turn) > _MAX_REFRESH_TURNS_TRACKED:
            state_by_turn.popitem(last=False)
        return st

    async def _refresh_memory(
        params: dict[str, Any], context: AgentContext
    ) -> ToolResult:
        diary_ref: AgentDiary | None = getattr(context, "agent_diary", None) or diary
        if diary_ref is None:
            return ToolResult(success=False, error="agent diary unavailable")
        if not context.agent_id:
            return ToolResult(
                success=False,
                error="agent_id not set on context — cannot scope memory query",
            )
        context_hint = str(params.get("context_hint", "")).strip()
        if not context_hint:
            return ToolResult(
                success=False,
                error=(
                    "context_hint is required — pass a short description of "
                    "what the conversation is actually about (the topic the "
                    "turn-start filter did not see)."
                ),
            )

        # Pull the TurnContext off AgentContext.__dict__ (the
        # TurnEngine attaches it for tools that need turn-scoped
        # state).  Without it we can't scope the per-turn cap or
        # surface the original task description -- both are essential.
        turn_ctx = getattr(context, "turn_context", None)
        if turn_ctx is None:
            return ToolResult(
                success=False,
                error=(
                    "turn context unavailable — refresh_memory must run during a turn"
                ),
            )
        turn_id = getattr(turn_ctx, "turn_id", "") or ""

        # Build the canonical interaction so the filter prompt's
        # ``Current sender:`` line still surfaces during a refresh, and
        # so ``original_task`` is the salient inbound message rather
        # than the notification builder's triage scaffolding -- the
        # ``context_hint`` is appended to ``original_task``, so if that
        # were the enriched task description the hint would land past
        # the filter's prompt-truncation window (the bug this fix
        # closes for the initial prefetch too).
        interactions = turn_ctx.trigger_interactions()
        original_task = salient_task_text(
            interactions,
            getattr(turn_ctx, "task_description", "")
            or getattr(turn_ctx, "original_task_description", "")
            or "",
        )

        role: Role | None = None
        if role_lookup is not None and context.agent_handle:
            try:
                role = role_lookup(context.agent_handle)
            except Exception:
                logger.exception(
                    "refresh_memory_role_lookup_failed", agent=context.agent_handle
                )
        if role is None:
            return ToolResult(
                success=False,
                error=(
                    "could not resolve role for the calling agent — "
                    "refresh_memory needs role.llm_auxiliary (or "
                    "role.llm) to drive the filter"
                ),
            )

        state = _state_for(turn_id)
        key = _hint_hash(context_hint)

        if key in state.cache:
            logger.debug(
                "refresh_memory_cache_hit",
                agent_handle=context.agent_handle,
                turn_id=turn_id,
            )
            return ToolResult(success=True, output=state.cache[key])

        if state.count >= cap:
            return ToolResult(
                success=False,
                error=(
                    f"refresh_memory cap reached for this turn ({cap} distinct "
                    "hints).  Repeated calls with the same hint are cached "
                    "and do not count.  Pick the most useful single context "
                    "and call once."
                ),
            )

        # Compose the enriched task description.  The planner's hint
        # is appended to the original task so the filter sees both --
        # we don't replace, because the original task is the trigger
        # the per-subject rules tie to.
        enriched_task = original_task or ""
        if enriched_task:
            enriched_task = (
                f"{enriched_task}\n\nContext gathered by planner: {context_hint}"
            )
        else:
            enriched_task = f"Context: {context_hint}"

        try:
            rendered = await fetch_personal_memory_block(
                diary=diary_ref,
                agent_id=context.agent_id,
                role=role,
                task_description=enriched_task,
                interactions=interactions,
                llm_providers=llm_providers,
                event_queue=getattr(context, "event_queue", None),
                turn_id=turn_id,
            )
        except Exception as exc:
            logger.exception("refresh_memory_filter_failed", turn_id=turn_id)
            return ToolResult(success=False, error=f"refresh failed: {exc}")

        # Record AFTER a successful filter run -- a failed refresh
        # shouldn't burn cap.  The cached output stands in for repeat
        # calls of the same hint regardless of whether anything matched.
        state.count += 1
        output = rendered or "(no relevant personal memories for that context)"
        state.cache[key] = output
        logger.info(
            "refresh_memory_completed",
            agent_handle=context.agent_handle,
            turn_id=turn_id,
            count=state.count,
            matched=bool(rendered),
        )
        return ToolResult(success=True, output=output)

    registry.register(
        SimpleTool(
            name="refresh_memory",
            description=(
                "Re-run the personal-memory relevance filter against "
                "your stored memories with context you have gathered "
                "during this turn. The initial Personal memory block "
                "in your system prompt was filtered against the "
                "triggering message at turn start, before any recon; "
                "once you have done recon (read a Slack thread, "
                "fetched a Jira ticket, looked up a counterparty, "
                "queried knowledge) you have a much richer view of "
                "what the conversation is about, and a fresh filter "
                "pass will surface different memories. Call once after "
                "meaningful recon -- even if the initial block already "
                "had entries. Pass `context_hint` as a 1-2 sentence "
                "summary of the enriched framing (the topic / "
                "situation the turn-start filter did not see). "
                "Repeated calls with the same hint are cached "
                f"for free; distinct calls are capped at {cap} per turn."
            ),
            parameters={
                "type": "object",
                "properties": {
                    "context_hint": {
                        "type": "string",
                        "description": (
                            "Short description (1-3 sentences) of what "
                            "the conversation is actually about, after "
                            "your recon.  Combined with the original "
                            "task before re-filtering."
                        ),
                    },
                },
                "required": ["context_hint"],
            },
            fn=_refresh_memory,
        )
    )
    logger.info("refresh_memory_tool_registered", cap=cap)
