"""SkillClusteringScheduler — periodic per-agent skill synthesis.

Runs on a configurable cadence (default 1h, opt-in).  For each agent
in the org:

1. Pulls recent successful episodes (``review_outcome=done``) within
   ``cluster_window_hours``.
2. Greedy-clusters them by tool-sequence Jaccard similarity.
3. For every cluster of size >= ``cluster_min_size`` whose
   representative sequence doesn't already match an existing
   synthesized skill, hands the cluster off to
   :class:`~crewlet.learning.skill_synthesizer.SkillSynthesizer`.

Follows the phase-2/3 best-effort convention: failures are logged and
swallowed, the scheduler keeps running.  Back-pressure: the loop
acquires a :class:`ConcurrencyController` slot per-agent before
synthesis so it never starves live turns.
"""

from __future__ import annotations

import asyncio
import contextlib
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import Event, SkillPromoted, SkillSynthesized
from crewlet.learning.episode_store import EpisodeStoreProtocol
from crewlet.learning.models import Episode, SynthesizedSkill
from crewlet.learning.skill_synthesizer import (
    PromotionSynthesizer,
    SkillSynthesizer,
    jaccard,
)
from crewlet.learning.synthesized_skill_store import SynthesizedSkillStoreProtocol
from crewlet.telemetry import set_span_error, tracer

logger = get_logger("learning.skill_scheduler")


class SkillClusteringScheduler:
    """Periodic scheduled skill-synthesis pass."""

    def __init__(
        self,
        *,
        synthesizer: SkillSynthesizer,
        episode_store: EpisodeStoreProtocol,
        agent_pool: Any,
        organization: Any,
        concurrency: Any = None,
        interval_seconds: int = 3600,
        cluster_window_hours: int = 168,
        cluster_min_size: int = 3,
        cluster_jaccard_threshold: float = 0.6,
        episode_fetch_limit: int = 200,
        # Cross-agent promotion pass.
        promotion_synthesizer: PromotionSynthesizer | None = None,
        synthesized_skill_store: SynthesizedSkillStoreProtocol | None = None,
        promotion_enabled: bool = True,
        promotion_min_sibling_count: int = 3,
        promotion_jaccard_threshold: float = 0.6,
        # Lifecycle events for the dashboard's trace-grouped view.
        event_queue: Any = None,
    ) -> None:
        self._synthesizer = synthesizer
        self._episode_store = episode_store
        self._agent_pool = agent_pool
        self._org = organization
        self._concurrency = concurrency
        self._event_queue = event_queue
        self._interval_seconds = max(1, int(interval_seconds))
        self._cluster_window_hours = max(1, int(cluster_window_hours))
        self._cluster_min_size = max(2, int(cluster_min_size))
        self._cluster_jaccard_threshold = float(cluster_jaccard_threshold)
        self._episode_fetch_limit = max(
            self._cluster_min_size, int(episode_fetch_limit)
        )
        # Promotion knobs
        self._promotion_synthesizer = promotion_synthesizer
        self._synthesized_skill_store = synthesized_skill_store
        self._promotion_enabled = bool(
            promotion_enabled
            and promotion_synthesizer is not None
            and synthesized_skill_store is not None
        )
        self._promotion_min_sibling_count = max(2, int(promotion_min_sibling_count))
        self._promotion_jaccard_threshold = float(promotion_jaccard_threshold)
        self._running = False
        self._task: asyncio.Task[None] | None = None

    async def start(self) -> None:
        if self._running:
            return
        self._running = True
        loop = asyncio.get_event_loop()
        self._task = loop.create_task(self._run_loop(), name="skill-scheduler")
        logger.info(
            "skill_scheduler_started",
            interval_seconds=self._interval_seconds,
            window_hours=self._cluster_window_hours,
            min_size=self._cluster_min_size,
        )

    async def stop(self) -> None:
        if not self._running:
            return
        self._running = False
        task = self._task
        self._task = None
        if task is not None:
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await task
        logger.info("skill_scheduler_stopped")

    async def _run_loop(self) -> None:
        """Loop until stopped; one tick per interval."""
        try:
            while self._running:
                try:
                    await asyncio.sleep(self._interval_seconds)
                except asyncio.CancelledError:
                    return
                if not self._running:
                    return
                try:
                    await self.tick_once()
                except asyncio.CancelledError:
                    return
                except Exception:
                    logger.exception("skill_scheduler_tick_failed")
        finally:
            self._running = False

    async def tick_once(self) -> int:
        """Run one pass over every agent, then one per unit.

        Returns the number of skills synthesised (agent + unit
        combined).  Exposed so tests can drive the scheduler without
        waiting on the real interval.
        """
        agents = list(getattr(self._agent_pool, "agents", []) or [])
        with tracer.start_as_current_span(
            "learning.skill_scheduler.tick",
            attributes={
                "learning.worker": "skill_scheduler",
                "learning.agent_count": len(agents),
                "learning.promotion_enabled": self._promotion_enabled,
            },
        ) as tick_span:
            synth_count = 0
            agent_synth = 0
            for agent in agents:
                try:
                    made = await self._tick_agent(agent)
                except Exception as exc:
                    set_span_error(exc)
                    logger.exception(
                        "skill_scheduler_agent_tick_failed",
                        agent_handle=getattr(agent, "handle", ""),
                    )
                    continue
                agent_synth += made
                synth_count += made

            unit_synth = 0
            if self._promotion_enabled:
                for unit in _iter_units(self._org):
                    try:
                        made = await self._tick_unit(unit, agents)
                    except Exception as exc:
                        set_span_error(exc)
                        logger.exception(
                            "skill_scheduler_unit_tick_failed",
                            unit_name=getattr(unit, "name", ""),
                        )
                        continue
                    unit_synth += made
                    synth_count += made

            tick_span.set_attribute("learning.agent_synthesised", agent_synth)
            tick_span.set_attribute("learning.unit_promoted", unit_synth)
            tick_span.set_attribute("learning.outcome", "done")
            logger.debug("skill_scheduler_tick_complete", synthesised=synth_count)
            return synth_count

    async def _tick_agent(self, agent: Any) -> int:
        handle = getattr(agent, "handle", "") or ""
        if not handle:
            return 0
        role = _resolve_role(self._org, agent)
        if role is None:
            logger.debug("skill_scheduler_skip_no_role", agent_handle=handle)
            return 0

        episodes = await self._episode_store.list_recent_by_outcome(
            agent_handle=handle,
            outcome="done",
            window_hours=self._cluster_window_hours,
            limit=self._episode_fetch_limit,
        )
        if len(episodes) < self._cluster_min_size:
            return 0

        clusters = _cluster_by_jaccard(
            episodes, threshold=self._cluster_jaccard_threshold
        )
        existing_names = collect_existing_skill_names()

        made = 0
        for cluster in clusters:
            if len(cluster) < self._cluster_min_size:
                continue
            with tracer.start_as_current_span(
                "learning.skill_synthesizer",
                attributes={
                    "learning.worker": "skill_synthesizer",
                    "learning.trigger": "clustered",
                    "learning.agent_handle": handle,
                    "learning.role": role.name,
                    "learning.cluster_size": len(cluster),
                },
            ) as worker_span:
                if self._concurrency is not None:
                    try:
                        await self._concurrency.acquire(role.name)
                    except Exception as exc:
                        worker_span.set_attribute("learning.outcome", "skipped")
                        worker_span.set_attribute(
                            "learning.skip_reason", "concurrency_acquire_failed"
                        )
                        set_span_error(exc)
                        logger.exception(
                            "skill_scheduler_acquire_failed", role=role.name
                        )
                        continue
                skill = None
                try:
                    skill = await self._synthesizer.synthesize(
                        role=role,
                        agent_handle=handle,
                        source_episodes=cluster,
                        existing_skill_names=existing_names,
                        trigger="clustered",
                        agent_id=getattr(agent, "id_str", "") or "",
                    )
                except Exception as exc:
                    worker_span.set_attribute("learning.outcome", "failed")
                    set_span_error(exc)
                    logger.exception(
                        "skill_scheduler_cluster_synth_failed",
                        agent_handle=handle,
                        cluster_size=len(cluster),
                    )
                finally:
                    if self._concurrency is not None:
                        try:
                            self._concurrency.release(role.name)
                        except Exception:
                            logger.exception(
                                "skill_scheduler_release_failed", role=role.name
                            )
                if skill is not None:
                    made += 1
                    existing_names = (*existing_names, skill.name)
                    worker_span.set_attribute("learning.outcome", "done")
                    worker_span.set_attribute("learning.skill_name", skill.name)
                    await self._publish_lifecycle(
                        SkillSynthesized(
                            source=role.name,
                            agent_handle=handle,
                            role=role.name,
                            skill_name=skill.name,
                            skill_id=str(skill.id),
                            trigger="clustered",
                            cluster_size=len(cluster),
                            tool_count=len(skill.tool_sequence or []),
                        )
                    )
                elif "learning.outcome" not in (worker_span.attributes or {}):
                    worker_span.set_attribute("learning.outcome", "noop")
        return made

    async def _tick_unit(self, unit: Any, agents: list[Any]) -> int:
        """Cross-agent promotion pass for one :class:`OrgUnit`.

        Collects every synthesized skill owned by an agent whose
        innermost unit is ``unit``, greedy-clusters them by
        tool-sequence Jaccard, and drafts a knowledge-base page (under
        the unit's ``Auto-Drafted Skills`` parent, through the active
        backend's ``PromotionPageWriter``) for each cluster of size >=
        ``promotion_min_sibling_count`` (measured in distinct agents).

        The engine stores no unit-scope rows -- the knowledge-base
        page is the artefact, reachable through the active backend's
        query-time search once a unit lead moves it out of the
        auto-draft parent.
        """
        if self._synthesized_skill_store is None or self._promotion_synthesizer is None:
            return 0
        unit_name = getattr(unit, "name", "") or ""
        if not unit_name:
            return 0
        member_handles = _member_handles_for_unit(unit, agents, self._org)
        if len(member_handles) < self._promotion_min_sibling_count:
            return 0
        store = self._synthesized_skill_store
        try:
            sibling_skills = await store.list_for_agent_handles(member_handles)
        except Exception:
            logger.exception(
                "skill_scheduler_unit_skills_fetch_failed", unit_name=unit_name
            )
            return 0
        if len(sibling_skills) < self._promotion_min_sibling_count:
            return 0

        clusters = _cluster_skills_by_jaccard(
            sibling_skills, threshold=self._promotion_jaccard_threshold
        )
        role = _representative_role_for_unit(unit, self._org)
        if role is None:
            logger.debug("skill_scheduler_unit_no_role", unit_name=unit_name)
            return 0

        # No reserved names beyond the agent's own -- cross-TICK dedup
        # of already-drafted pages is the page writer's job, per backend:
        # Confluence rejects a repeat create with a 4xx on the
        # duplicate title; the Plane writer stamps
        # external_id="draft:<name>" (the fork 409s on the duplicate
        # pair) and returns the existing page instead of creating.
        # ``reserved`` below only dedups names WITHIN one tick.
        reserved: set[str] = set()
        made = 0
        for cluster in clusters:
            distinct_agents = {s.agent_handle for s in cluster if s.agent_handle}
            if len(distinct_agents) < self._promotion_min_sibling_count:
                continue
            with tracer.start_as_current_span(
                "learning.skill_promotion",
                attributes={
                    "learning.worker": "skill_promotion",
                    "learning.unit_id": unit_name,
                    "learning.role": role.name,
                    "learning.cluster_size": len(cluster),
                    "learning.distinct_agents": len(distinct_agents),
                },
            ) as worker_span:
                promoted = None
                try:
                    promoted = await self._promotion_synthesizer.synthesize_promotion(
                        role=role,
                        unit_id=unit_name,
                        source_skills=cluster,
                        existing_skill_names=tuple(reserved),
                    )
                except Exception as exc:
                    worker_span.set_attribute("learning.outcome", "failed")
                    set_span_error(exc)
                    logger.exception(
                        "skill_scheduler_promotion_failed", unit_name=unit_name
                    )
                    continue
                if promoted is not None:
                    made += 1
                    reserved.add(promoted.skill_name)
                    worker_span.set_attribute("learning.outcome", "done")
                    worker_span.set_attribute(
                        "learning.skill_name", promoted.skill_name
                    )
                    worker_span.set_attribute("learning.page_id", promoted.page_id)
                    await self._publish_lifecycle(
                        SkillPromoted(
                            source=role.name,
                            role=role.name,
                            unit_id=unit_name,
                            skill_name=promoted.skill_name,
                            page_id=promoted.page_id,
                            page_title=promoted.page_title,
                            container_key=promoted.container_key,
                            sibling_count=len(cluster),
                            distinct_agents=len(distinct_agents),
                        )
                    )
                else:
                    worker_span.set_attribute("learning.outcome", "noop")
        return made

    async def _publish_lifecycle(self, event: Event) -> None:
        """Publish a learning lifecycle event when ``event_queue`` is wired.

        Best-effort: a queue failure must not break the scheduler tick.
        Trace context is auto-captured by the ``Event`` base from the
        active OTel span (the surrounding ``learning.skill_synthesizer``
        or ``learning.skill_promotion`` span), so the dashboard's
        trace-grouped view can show scheduled work alongside its tick.
        """
        if self._event_queue is None:
            return
        try:
            await self._event_queue.publish(f"crewlet.events.{event.type}", event)
        except Exception:
            logger.exception("learning_event_publish_failed", event_type=event.type)


# --------------------------------------------------------------------
# Clustering
# --------------------------------------------------------------------


def _cluster_by_jaccard(
    episodes: list[Episode], *, threshold: float
) -> list[list[Episode]]:
    """Greedy single-pass clustering on tool-sequence Jaccard.

    Deliberately simple: walk episodes in time order; each one
    joins the first existing cluster whose representative sequence
    has Jaccard >= ``threshold`` with it, or starts a new cluster.
    Good enough to catch "same task done three times" without the
    mass of a real hierarchical clusterer.
    """
    clusters: list[list[Episode]] = []
    for episode in episodes:
        if not episode.tool_sequence:
            continue
        placed = False
        for cluster in clusters:
            rep = cluster[0].tool_sequence
            if jaccard(list(episode.tool_sequence), list(rep)) >= threshold:
                cluster.append(episode)
                placed = True
                break
        if not placed:
            clusters.append([episode])
    return clusters


def _resolve_role(org: Any, agent: Any) -> Any:
    """Resolve the agent's Role via the org, tolerating stub orgs."""
    try:
        role = org.get_role(agent.role_name)
    except Exception:
        return None
    if role is not None:
        return role
    # Some lightweight test orgs don't index by role name; fall back
    # to the agent's statically-attached definition.role.
    definition = getattr(agent, "definition", None)
    if definition is not None:
        return getattr(definition, "role", None)
    return None


def collect_existing_skill_names() -> tuple[str, ...]:
    """Extra reserved names to feed the synthesizer, beyond the agent's own.

    ``SkillSynthesizer.synthesize`` self-fetches the agent's own
    existing skill names from the store (so the drafting LLM sees them
    and the post-draft collision check is real).  This helper is the
    seam for any *additional* shared-name space -- there is no shared
    skill-name space today, so it returns ``()``.  Kept as an explicit
    hook in case a future shared registry shows up.
    """
    return ()


def _cluster_skills_by_jaccard(
    skills: list[SynthesizedSkill], *, threshold: float
) -> list[list[SynthesizedSkill]]:
    """Greedy cluster of skills by tool-sequence Jaccard.

    Mirror of :func:`_cluster_by_jaccard` but over skills rather than
    episodes; used by the promotion pass so clusters preserve the
    ``SynthesizedSkill`` objects (the promotion synthesizer needs
    the names + bodies of the source skills, not episode summaries).
    """
    clusters: list[list[SynthesizedSkill]] = []
    for skill in skills:
        if not skill.tool_sequence:
            continue
        placed = False
        for cluster in clusters:
            rep = cluster[0].tool_sequence
            if jaccard(list(skill.tool_sequence), list(rep)) >= threshold:
                cluster.append(skill)
                placed = True
                break
        if not placed:
            clusters.append([skill])
    return clusters


def _iter_units(org: Any) -> list[Any]:
    """Return every :class:`OrgUnit` in the org tree, tolerating stubs."""
    if org is None:
        return []
    try:
        return list(org.all_units())
    except Exception:
        return []


def _member_handles_for_unit(unit: Any, agents: list[Any], org: Any) -> list[str]:
    """Handles of agents whose innermost unit is ``unit``.

    Derives membership from the agent's ``role_name`` and the unit's
    declared ``roles`` list.  Child units are not included — promotion
    operates at a single-unit granularity.
    """
    unit_role_names: set[str] = set()
    for entry in getattr(unit, "roles", []) or []:
        name = getattr(entry, "name", "") or (entry if isinstance(entry, str) else "")
        if name:
            unit_role_names.add(name)
    if not unit_role_names:
        return []
    handles: list[str] = []
    for agent in agents:
        role_name = getattr(agent, "role_name", "") or ""
        if role_name in unit_role_names:
            handle = getattr(agent, "handle", "") or ""
            if handle:
                handles.append(handle)
    return handles


def _representative_role_for_unit(unit: Any, org: Any) -> Any:
    """Pick any role attached to ``unit`` to use for aux-LLM routing.

    Prefers the unit's declared lead; falls back to the first
    direct role.  Returns ``None`` when the unit has no direct roles.
    """
    lead_name = getattr(unit, "lead", "") or ""
    direct_roles = list(getattr(unit, "roles", []) or [])
    if lead_name:
        for entry in direct_roles:
            role_name = getattr(entry, "name", "") or (
                entry if isinstance(entry, str) else ""
            )
            if role_name == lead_name:
                # If ``entry`` is a Role instance return it directly.
                if hasattr(entry, "name"):
                    return entry
                # Otherwise resolve via the org.
                if org is not None:
                    try:
                        return org.get_role(lead_name)
                    except Exception:
                        pass
    for entry in direct_roles:
        if hasattr(entry, "name"):
            return entry
        if isinstance(entry, str) and org is not None:
            try:
                return org.get_role(entry)
            except Exception:
                continue
    return None


__all__ = ["SkillClusteringScheduler"]
