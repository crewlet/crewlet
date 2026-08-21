"""EpisodeLifecycleWorker — drains the ``episodes`` table.

Subscribes to ``crewlet.events.compaction_requested`` published by
:class:`~crewlet.learning.episode_store.EpisodeStore` when an agent's
raw episode count crosses ``max_raw_episodes_per_agent``.  For each
event, runs the full per-agent lifecycle pass:

1. **Drop non-terminal episodes** older than
   ``non_terminal_max_age_days``.  ``self_iterate`` is a mid-state; the
   reflect engine's terminal-outcome gate already excludes it from
   skill synthesis, so it only contributes noise to ``query_episodes``
   recall.  Cheap SQL DELETE; no LLM.
2. **Drop skill-consolidated episodes** older than
   ``consolidated_grace_days``.  Stamped by ``SkillSynthesizer`` when
   they contributed to a skill draft -- the skill carries the learning
   forward, so the raw row is redundant for future planning.  Grace
   gives a chance to audit / detect bad consolidations before the
   source disappears.  Cheap SQL DELETE.
3. **Compact remaining raw episodes** older than
   ``compaction_min_age_days``: cluster by tool-sequence Jaccard
   (same scorer used by the synthesizer), summarise each cluster of
   size >= ``compaction_min_cluster_size`` via the role's
   ``llm_auxiliary``, write one ``kind='compacted'`` row per cluster,
   delete the originals (except 2-3 exemplars retained for
   drill-down).  This is the LLM-driven step.
4. **Optional: evict ancient compacted entries** older than
   ``compacted_max_age_days``.  Default 0 (disabled); turn on for hard
   long-tail storage caps.

Per-agent semaphore so a burst of ``CompactionRequested`` events for
the same agent doesn't fan out overlapping passes.  Best-effort: a
failed pass logs and short-circuits but never breaks the engine.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import re
from collections.abc import Iterable
from datetime import UTC, datetime
from typing import Any
from uuid import UUID, uuid4

from crewlet._logging import get_logger
from crewlet.agent.phase_model import resolve_phase_provider
from crewlet.events.types import CompactionCompleted, CompactionRequested, Event
from crewlet.learning._aux_telemetry import complete_with_telemetry
from crewlet.learning.episode_store import EpisodeStoreProtocol
from crewlet.learning.models import Episode
from crewlet.org.models import Organization, Role
from crewlet.providers.llm.protocol import LLMProvider, Message
from crewlet.queue.protocol import EventQueue

logger = get_logger("learning.episode_lifecycle")

COMPACTION_TOPIC = "crewlet.events.compaction_requested"
CONSUMER_GROUP = "episode-lifecycle-worker"

# Compaction prompt clamps per-turn detail to this many chars so a
# cluster of N=100 turns doesn't exceed the aux LLM's context window.
_PER_TURN_DETAIL_CHARS = 280

# Number of original episodes to retain as raw rows after compaction
# (exemplars referenced by the new compacted row's
# ``exemplar_turn_ids``) so operators can still drill into a typical
# example of the cluster's pattern.
_DEFAULT_EXEMPLAR_COUNT = 2

_COMPACTOR_SYSTEM_PROMPT = """You are an agent-learning episode compactor.

You receive N similar agent turns (each a brief turn summary) and must
emit ONE JSON object summarising the recurring pattern.  The output
becomes a single compacted episode row that replaces the N originals
in the agent's episode store.

Output format (strict, JSON only -- no prose before or after):

{
  "common_task_pattern": "<one sentence describing the recurring task>",
  "common_outcome": "done" | "failed",
  "success_rate": <float 0.0..1.0>,
  "subjects_involved": ["<distinct counterparties / subjects>"],
  "notable_patterns": "<one to three sentences naming variations / edge cases>"
}

Rules:
- ``common_task_pattern`` is declarative and short; the planner reads
  it as "you've done this kind of work N times".
- ``subjects_involved`` lists distinct named counterparties (Slack
  user labels, Jira reporter handles).  Empty if none consistent.
- ``notable_patterns`` captures variation across the cluster
  (e.g. "most replied within the thread; 2 handed off to manager
  when message contained 'urgent'").  Keep it useful, not exhaustive.
- Never invent facts not present in the turns.
- If the turns don't actually share a coherent pattern, emit
  ``{"common_task_pattern": "(heterogeneous)"}``; the rest of the
  fields can be empty -- the compactor will still write the row but
  the planner will see it as low-signal.
"""


class EpisodeLifecycleWorker:
    """Subscribes to ``CompactionRequested`` and drains the agent's pool.

    Wired into :meth:`crewlet.engine.Engine.start` after the EpisodeStore
    + ReflectEngine.  Owns its own subscription; does not share a
    consumer group with any other subsystem.
    """

    def __init__(
        self,
        *,
        event_queue: EventQueue,
        budget_manager: Any = None,
        episode_store: EpisodeStoreProtocol,
        llm_providers: dict[str, LLMProvider],
        organization: Organization,
        agent_pool: Any,
        non_terminal_max_age_days: int = 14,
        consolidated_grace_days: int = 30,
        compaction_min_age_days: int = 30,
        compaction_min_cluster_size: int = 3,
        compaction_jaccard_threshold: float = 0.6,
        compaction_batch_size: int = 200,
        compaction_budget_tokens: int = 4000,
        compacted_max_age_days: int = 0,
        exemplar_count: int = _DEFAULT_EXEMPLAR_COUNT,
    ) -> None:
        self._event_queue = event_queue
        self._budget_manager = budget_manager
        self._episode_store = episode_store
        self._llm_providers = llm_providers
        self._org = organization
        self._agent_pool = agent_pool
        self._non_terminal_max_age_days = max(0, int(non_terminal_max_age_days))
        self._consolidated_grace_days = max(0, int(consolidated_grace_days))
        self._compaction_min_age_days = max(1, int(compaction_min_age_days))
        self._compaction_min_cluster_size = max(2, int(compaction_min_cluster_size))
        self._compaction_jaccard_threshold = float(compaction_jaccard_threshold)
        self._compaction_batch_size = max(
            self._compaction_min_cluster_size, int(compaction_batch_size)
        )
        self._compaction_budget_tokens = max(0, int(compaction_budget_tokens))
        self._compacted_max_age_days = max(0, int(compacted_max_age_days))
        self._exemplar_count = max(0, int(exemplar_count))
        # Per-agent semaphore: dedup concurrent requests for the same
        # agent so a burst of writes doesn't fan out overlapping passes.
        self._agent_locks: dict[str, asyncio.Lock] = {}
        self._running = False
        self._subscribed = False

    async def start(self) -> None:
        if self._running:
            return
        self._running = True
        if not self._subscribed:
            await self._event_queue.subscribe(
                COMPACTION_TOPIC, CONSUMER_GROUP, self._handle
            )
            self._subscribed = True
        logger.info(
            "episode_lifecycle_started",
            non_terminal_max_age_days=self._non_terminal_max_age_days,
            consolidated_grace_days=self._consolidated_grace_days,
            compaction_min_age_days=self._compaction_min_age_days,
            compaction_min_cluster_size=self._compaction_min_cluster_size,
            compacted_max_age_days=self._compacted_max_age_days,
        )

    async def stop(self) -> None:
        if not self._running:
            return
        self._running = False
        logger.info("episode_lifecycle_stopped")

    # ------------------------------------------------------------------
    # Event handler
    # ------------------------------------------------------------------

    async def _handle(self, event: Event) -> None:
        if not self._running:
            return
        if not isinstance(event, CompactionRequested):
            # Defensive: queue backends sometimes deserialise to base
            # Event; reconstruct from payload.
            try:
                event = CompactionRequested.model_validate(
                    event.model_dump(mode="python")
                )
            except Exception:
                logger.debug("episode_lifecycle_skipped_unparseable_event")
                return
        agent_handle = event.agent_handle
        if not agent_handle:
            return
        lock = self._agent_locks.setdefault(agent_handle, asyncio.Lock())
        if lock.locked():
            # Another pass already running for this agent; the in-progress
            # one will see the latest state when it queries.  Skip this
            # event silently rather than queue up a redundant pass.
            await self._publish_completed(agent_handle, skipped="already_running")
            return
        async with lock:
            try:
                await self._run_pass(agent_handle)
            except Exception:
                logger.exception(
                    "episode_lifecycle_pass_failed", agent_handle=agent_handle
                )

    # ------------------------------------------------------------------
    # Lifecycle pass
    # ------------------------------------------------------------------

    async def _run_pass(self, agent_handle: str) -> None:
        """One full per-agent lifecycle pass."""
        # Action 1: drop non-terminal mid-state rows.
        non_terminal_dropped = 0
        if self._non_terminal_max_age_days > 0:
            try:
                non_terminal_dropped = (
                    await self._episode_store.delete_non_terminal_older_than_days(
                        agent_handle=agent_handle,
                        days=self._non_terminal_max_age_days,
                    )
                )
            except Exception:
                logger.exception(
                    "episode_lifecycle_drop_non_terminal_failed",
                    agent_handle=agent_handle,
                )

        # Action 2: drop skill-consolidated rows past grace.
        consolidated_dropped = 0
        if self._consolidated_grace_days > 0:
            try:
                consolidated_dropped = (
                    await self._episode_store.delete_consolidated_older_than_days(
                        agent_handle=agent_handle,
                        days=self._consolidated_grace_days,
                    )
                )
            except Exception:
                logger.exception(
                    "episode_lifecycle_drop_consolidated_failed",
                    agent_handle=agent_handle,
                )

        # Action 3: cluster + LLM-compact remaining raw episodes.
        clusters_compacted = 0
        raw_replaced = 0
        try:
            clusters_compacted, raw_replaced = await self._compact_routine(agent_handle)
        except Exception:
            logger.exception(
                "episode_lifecycle_compaction_failed", agent_handle=agent_handle
            )

        # Action 4 (optional): evict very old compacted entries.
        compacted_evicted = 0
        if self._compacted_max_age_days > 0:
            try:
                compacted_evicted = (
                    await self._episode_store.delete_compacted_older_than_days(
                        agent_handle=agent_handle,
                        days=self._compacted_max_age_days,
                    )
                )
            except Exception:
                logger.exception(
                    "episode_lifecycle_evict_compacted_failed",
                    agent_handle=agent_handle,
                )

        logger.info(
            "episode_lifecycle_pass_completed",
            agent_handle=agent_handle,
            non_terminal_dropped=non_terminal_dropped,
            consolidated_dropped=consolidated_dropped,
            clusters_compacted=clusters_compacted,
            raw_replaced_by_compaction=raw_replaced,
            compacted_evicted=compacted_evicted,
        )
        await self._publish_completed(
            agent_handle,
            non_terminal_dropped=non_terminal_dropped,
            consolidated_dropped=consolidated_dropped,
            clusters_compacted=clusters_compacted,
            raw_replaced=raw_replaced,
            compacted_evicted=compacted_evicted,
        )

    async def _compact_routine(self, agent_handle: str) -> tuple[int, int]:
        """Cluster + LLM-summarise + write + delete.  Returns
        ``(clusters_compacted, raw_replaced)``."""
        candidates = await self._episode_store.fetch_raw_for_compaction(
            agent_handle=agent_handle,
            older_than_days=self._compaction_min_age_days,
            limit=self._compaction_batch_size,
        )
        if len(candidates) < self._compaction_min_cluster_size:
            return 0, 0
        clusters = _cluster_by_jaccard(
            candidates, threshold=self._compaction_jaccard_threshold
        )
        eligible = [c for c in clusters if len(c) >= self._compaction_min_cluster_size]
        if not eligible:
            return 0, 0
        role = self._resolve_any_role(agent_handle)
        if role is None:
            logger.debug("episode_lifecycle_no_role", agent_handle=agent_handle)
            return 0, 0
        try:
            provider_key, provider = resolve_phase_provider(
                role, "auxiliary", self._llm_providers
            )
        except Exception:
            logger.debug("episode_lifecycle_no_aux_provider", agent_handle=agent_handle)
            return 0, 0

        clusters_compacted = 0
        raw_replaced = 0
        for cluster in eligible:
            try:
                processed = await self._compact_one_cluster(
                    agent_handle=agent_handle,
                    cluster=cluster,
                    role=role,
                    provider=provider,
                    provider_key=provider_key,
                )
            except Exception:
                logger.exception(
                    "episode_lifecycle_cluster_compaction_failed",
                    agent_handle=agent_handle,
                    cluster_size=len(cluster),
                )
                continue
            if processed > 0:
                clusters_compacted += 1
                raw_replaced += processed
        return clusters_compacted, raw_replaced

    async def _compact_one_cluster(
        self,
        *,
        agent_handle: str,
        cluster: list[Episode],
        role: Role,
        provider: LLMProvider,
        provider_key: str,
    ) -> int:
        """Compact one cluster.  Returns count of raw rows deleted
        (= cluster size minus retained exemplars)."""
        exemplar_count = min(self._exemplar_count, len(cluster) - 1)
        # Keep the most-recent exemplars so drill-down samples are
        # representative of the latest behaviour.
        sorted_by_recent = sorted(cluster, key=lambda e: e.ended_at, reverse=True)
        exemplars = sorted_by_recent[:exemplar_count]
        exemplar_ids = {ep.id for ep in exemplars}
        to_delete = [ep for ep in cluster if ep.id not in exemplar_ids]
        if not to_delete:
            return 0

        user_prompt = _build_compactor_user_prompt(cluster)
        try:
            completion = await complete_with_telemetry(
                provider=provider,
                messages=[
                    Message(role="system", content=_COMPACTOR_SYSTEM_PROMPT),
                    Message(role="user", content=user_prompt),
                ],
                worker="episode_compactor",
                role_name=role.name,
                provider_key=provider_key,
                event_queue=self._event_queue,
                budget_manager=self._budget_manager,
                agent_id="",
                turn_id="",
                temperature=0.2,
                max_tokens=(self._compaction_budget_tokens or None),
            )
        except Exception:
            logger.exception(
                "episode_compactor_llm_failed",
                agent_handle=agent_handle,
                cluster_size=len(cluster),
            )
            return 0

        parsed = _extract_json_object((completion.content or "").strip())
        if parsed is None:
            logger.warning(
                "episode_compactor_unparseable",
                agent_handle=agent_handle,
                preview=(completion.content or "")[:200],
            )
            return 0

        compacted = _build_compacted_episode(
            agent_handle=agent_handle,
            agent_role=cluster[0].agent_role,
            cluster=cluster,
            exemplar_ids=[ep.id for ep in exemplars],
            parsed=parsed,
        )
        await self._episode_store.insert_compacted(compacted)
        deleted = await self._episode_store.delete_episodes_by_ids(
            [str(ep.id) for ep in to_delete]
        )
        logger.info(
            "episode_cluster_compacted",
            agent_handle=agent_handle,
            cluster_size=len(cluster),
            exemplars_kept=len(exemplars),
            raw_deleted=deleted,
            common_task_pattern=compacted.common_task_pattern[:120],
        )
        return deleted

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    def _resolve_any_role(self, agent_handle: str) -> Role | None:
        """Resolve the agent's role so the aux provider can be selected.

        From the ORG first, and only then from the pool. This worker
        consumes a fleet-wide group, so the node that wins a
        ``CompactionRequested`` is rarely the node running that seat —
        and a pool-first lookup answered "not here" for every one of
        them, silently downgrading the compaction to the default aux
        provider instead of the role's configured one. The org knows
        every seat on every node; the pool is an execution detail.

        The pool fallback stays for lightweight test orgs that carry a
        definition but no indexed role.
        """
        if not agent_handle:
            return None
        seat = None
        try:
            seat = self._org.agent_seat_by_handle(agent_handle)
        except Exception:
            seat = None
        if seat is not None:
            return seat
        agent = None
        if self._agent_pool is not None:
            try:
                agent = self._agent_pool.get_by_handle(agent_handle)
            except Exception:
                agent = None
        if agent is None:
            return None
        try:
            role = self._org.get_role(agent.role_name)
        except Exception:
            role = None
        if role is not None:
            return role
        # Fall back to the agent's statically-attached definition.role
        # for stub orgs that don't index by role name.
        definition = getattr(agent, "definition", None)
        if definition is not None:
            return getattr(definition, "role", None)
        return None

    async def _publish_completed(
        self,
        agent_handle: str,
        *,
        non_terminal_dropped: int = 0,
        consolidated_dropped: int = 0,
        clusters_compacted: int = 0,
        raw_replaced: int = 0,
        compacted_evicted: int = 0,
        skipped: str = "",
    ) -> None:
        try:
            await self._event_queue.publish(
                "crewlet.events.compaction_completed",
                CompactionCompleted(
                    source="episode_lifecycle",
                    agent_handle=agent_handle,
                    non_terminal_dropped=non_terminal_dropped,
                    consolidated_dropped=consolidated_dropped,
                    clusters_compacted=clusters_compacted,
                    raw_replaced_by_compaction=raw_replaced,
                    compacted_evicted=compacted_evicted,
                    skipped_reason=skipped,
                ),
            )
        except Exception:
            logger.exception(
                "episode_lifecycle_publish_failed", agent_handle=agent_handle
            )


# --------------------------------------------------------------------
# Pure helpers (testable without a full worker)
# --------------------------------------------------------------------


def _cluster_by_jaccard(
    episodes: list[Episode], *, threshold: float
) -> list[list[Episode]]:
    """Greedy single-pass clustering on tool-sequence Jaccard.

    Mirrors the helper in :mod:`skill_scheduler` (intentionally:
    same algorithm so episodes that *would* be a skill-cluster are
    also a compaction-cluster, and vice versa).  Inlined here rather
    than imported to keep the lifecycle module independent of the
    skill-scheduler module.
    """
    clusters: list[list[Episode]] = []
    for episode in episodes:
        if not episode.tool_sequence:
            continue
        placed = False
        for cluster in clusters:
            rep = cluster[0].tool_sequence
            if _jaccard(episode.tool_sequence, rep) >= threshold:
                cluster.append(episode)
                placed = True
                break
        if not placed:
            clusters.append([episode])
    return clusters


def _jaccard(a: Iterable[str], b: Iterable[str]) -> float:
    """Set Jaccard over tool-sequence lists.  Empty either side -> 0."""
    sa, sb = set(a), set(b)
    if not sa or not sb:
        return 0.0
    return len(sa & sb) / len(sa | sb)


def _build_compactor_user_prompt(cluster: list[Episode]) -> str:
    """Render a cluster as a numbered list of brief turn summaries
    for the compactor LLM.  Per-turn detail is clamped so a 100-turn
    cluster doesn't blow the model's context."""
    lines = [
        f"Cluster of {len(cluster)} similar agent turns from one agent.",
        "",
        "Turns (numbered):",
    ]
    for i, ep in enumerate(cluster):
        task = (ep.task_summary or "").strip().replace("\n", " ")
        plan = (ep.plan_summary or "").strip().replace("\n", " ")
        if len(task) > _PER_TURN_DETAIL_CHARS:
            task = task[:_PER_TURN_DETAIL_CHARS] + "..."
        if len(plan) > _PER_TURN_DETAIL_CHARS:
            plan = plan[:_PER_TURN_DETAIL_CHARS] + "..."
        tools = ", ".join(ep.tool_sequence[:8]) or "(none)"
        lines.append(f"{i}. [{ep.review_outcome}] task: {task}")
        lines.append(f"   plan: {plan}")
        lines.append(f"   tools: {tools}")
        lines.append(f"   when: {ep.ended_at.date().isoformat()}")
    lines.append("")
    lines.append(
        "Emit one JSON object summarising the recurring pattern across "
        "these turns.  Strict JSON, no prose."
    )
    return "\n".join(lines)


def _extract_json_object(text: str) -> dict[str, Any] | None:
    """Best-effort JSON-object extraction (mirrors
    PersistDecider's tolerance of prose-wrapped output)."""
    candidates: list[str] = [text]
    start = text.find("{")
    end = text.rfind("}")
    if start != -1 and end != -1 and end > start:
        candidates.append(text[start : end + 1])
    for candidate in candidates:
        try:
            obj = json.loads(candidate)
        except Exception:
            continue
        if isinstance(obj, dict):
            return obj
    return None


def _build_compacted_episode(
    *,
    agent_handle: str,
    agent_role: str,
    cluster: list[Episode],
    exemplar_ids: list[UUID],
    parsed: dict[str, Any],
) -> Episode:
    """Construct the ``kind='compacted'`` Episode from the LLM's
    parsed JSON + the cluster's metadata.  Computes
    ``success_rate`` from the cluster's actual outcomes (don't trust
    the LLM's value if the model invented one)."""
    pattern = str(parsed.get("common_task_pattern") or "(unspecified)").strip()
    common_outcome = str(parsed.get("common_outcome") or "").strip()
    notable = str(parsed.get("notable_patterns") or "").strip()
    subjects_raw = parsed.get("subjects_involved") or []
    if isinstance(subjects_raw, list):
        subjects = [str(s).strip() for s in subjects_raw if str(s).strip()]
    else:
        subjects = []
    # Compute success rate from actual cluster outcomes -- LLM-supplied
    # value can drift; the truth is in the data we just clustered.
    done_count = sum(1 for ep in cluster if ep.review_outcome == "done")
    success_rate = done_count / len(cluster) if cluster else 0.0
    started_at = min(ep.started_at for ep in cluster)
    ended_at = max(ep.ended_at for ep in cluster)
    # Use the most-common tool sequence (first cluster element is fine
    # under greedy clustering -- it's the cluster's representative).
    common_tool_sequence = list(cluster[0].tool_sequence)
    # Total duration is sum of constituent durations; useful as an
    # aggregate metric in `query_episodes` rendering.
    total_duration = sum(ep.duration_ms for ep in cluster)
    return Episode(
        id=uuid4(),
        agent_handle=agent_handle,
        agent_role=agent_role,
        task_id="",
        turn_id=uuid4(),  # synthetic; compacted rows aren't keyed by turn
        started_at=started_at,
        ended_at=ended_at,
        plan_summary="",
        task_summary="",
        tool_sequence=common_tool_sequence,
        skills_used=[],
        # Derive the indexed ``review_outcome`` from the *data* -- the
        # cluster's actual success rate -- not the LLM's
        # ``common_outcome`` string.  ``query_episodes`` and
        # ``list_recent_by_outcome`` filter on this column; trusting the
        # LLM here let a cluster that was 90% ``failed`` surface
        # under ``outcome_filter="done"`` when the LLM mislabelled it.
        review_outcome="done" if success_rate >= 0.5 else "failed",
        duration_ms=total_duration,
        kind="compacted",
        count=len(cluster),
        exemplar_turn_ids=list(exemplar_ids),
        consolidated_into_skill_id=None,
        common_task_pattern=pattern,
        common_outcome=common_outcome,
        success_rate=success_rate,
        subjects_involved=subjects,
        notable_patterns=notable,
    )


# Re-exported for tests and operators that need the constants.
__all__ = [
    "EpisodeLifecycleWorker",
    "_cluster_by_jaccard",
    "_build_compactor_user_prompt",
    "_extract_json_object",
    "_build_compacted_episode",
    "COMPACTION_TOPIC",
    "CONSUMER_GROUP",
]


# Silence unused-import warnings for symbols imported for downstream use.
_ = (UTC, datetime, contextlib, re)
