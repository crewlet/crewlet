"""SkillCuratorWorker — interval-driven skill-state auto-transition.

Synthesized skills (``synthesized_skills`` rows) accumulate forever
without a lifecycle. This worker walks the table on an interval (default
24h) and transitions stale skills out of the Plan-phase catalogue:

- ``active`` → ``stale`` once a skill has gone unused for
  ``stale_after_days`` (default 30).
- ``stale`` → ``archived`` once a stale skill has *still* gone unused
  for ``archive_after_days`` (default 90).
- ``stale`` → ``active`` when the agent uses it again (revival).

Pinned skills are exempt from every auto-transition. Archived skills
are never deleted -- the curator only flips the ``state`` flag. The
Plan-phase prefetch hides archived rows; ``_use_skill`` refuses to
load them.

**Read-then-archive race.** The Plan prefetch caches the agent's
skill list at turn start; if the curator archives a skill mid-turn,
the agent's ``use_skill`` then fails. The fix lives in the store:
``transition_state`` takes an optimistic-concurrency guard
(``guard_last_used_at``) and only fires the demote UPDATE if the
row's ``last_used_at`` still equals what the curator snapshotted via
``list_curator_candidates``. A concurrent ``mark_used`` that bumps
``last_used_at`` makes the guarded UPDATE a no-op; the curator counts
that as ``skipped_raced`` and leaves the just-used skill alone.
Revival (``stale → active``) is unguarded -- reviving a recently-used
skill is the safe direction.
"""

from __future__ import annotations

import asyncio
import contextlib
from datetime import UTC, datetime, timedelta
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import SkillArchived, SkillRevived, SkillStaled
from crewlet.learning.models import SynthesizedSkill
from crewlet.learning.synthesized_skill_store import SynthesizedSkillStoreProtocol

logger = get_logger("learning.skill_curator")


class SkillCuratorWorker:
    """Interval-driven curator for ``synthesized_skills`` lifecycle."""

    def __init__(
        self,
        *,
        store: SynthesizedSkillStoreProtocol,
        event_queue: Any = None,
        interval_hours: int = 24,
        stale_after_days: int = 30,
        archive_after_days: int = 90,
        # Fleet-wide singleton gate. The curator TRANSITIONS rows —
        # active to stale to archived — and publishes a lifecycle event
        # per transition, so N nodes produce N event streams for one
        # set of changes and race each other's writes. Supplied by the
        # engine as a callable that claims ``worker:skill-curator``;
        # ``None`` means "no fleet".
        claim_duty: Any = None,
    ) -> None:
        self._store = store
        self._event_queue = event_queue
        # Test paths sometimes pass fractional intervals (0.01h ≈ 36s)
        # to verify the loop fires; clamp to a positive float without
        # forcing integer arithmetic.
        self._interval_seconds = max(1.0, float(interval_hours) * 3600.0)
        self._stale_after_days = max(1, int(stale_after_days))
        self._archive_after_days = max(self._stale_after_days, int(archive_after_days))
        self._claim_duty = claim_duty
        self._running = False
        self._task: asyncio.Task[None] | None = None

    async def start(self) -> None:
        if self._running:
            return
        self._running = True
        loop = asyncio.get_running_loop()
        self._task = loop.create_task(self._run_loop(), name="skill-curator")
        logger.info(
            "skill_curator_started",
            interval_seconds=self._interval_seconds,
            stale_after_days=self._stale_after_days,
            archive_after_days=self._archive_after_days,
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
        logger.info("skill_curator_stopped")

    async def _run_loop(self) -> None:
        try:
            while self._running:
                try:
                    await asyncio.sleep(self._interval_seconds)
                except asyncio.CancelledError:
                    return
                if not self._running:
                    return
                try:
                    if not await self._may_tick():
                        continue
                    await self.tick_once()
                except asyncio.CancelledError:
                    return
                except Exception:
                    logger.exception("skill_curator_tick_failed")
        finally:
            self._running = False

    async def _may_tick(self) -> bool:
        """Whether this node holds this duty for this tick.

        Re-claimed every tick rather than held: the duty lease is short,
        so a node that dies mid-pass releases it by lapsing and a peer
        takes over on its next tick, with no handoff protocol. ``None``
        means "no fleet" — the single-node case, where there is nothing
        to be a singleton within.
        """
        if self._claim_duty is None:
            return True
        try:
            return bool(await self._claim_duty())
        except Exception:
            logger.exception("skill_curator_duty_claim_failed")
            return False

    async def tick_once(self, *, now: datetime | None = None) -> dict[str, int]:
        """Run one pass: scan candidates, apply transitions, publish
        events. Returns a counts dict useful for tests / dashboards
        (``staled`` / ``archived`` / ``revived`` / ``skipped_raced``).

        ``skipped_raced`` counts demote transitions the curator
        attempted but whose guarded UPDATE no-op'd because a
        concurrent ``mark_used`` bumped the skill's ``last_used_at``
        between the candidate SELECT and the UPDATE.
        """
        wall_now = now or datetime.now(UTC)
        try:
            candidates = await self._store.list_curator_candidates()
        except Exception:
            logger.exception("skill_curator_list_failed")
            return {"staled": 0, "archived": 0, "revived": 0, "skipped_raced": 0}

        stale_cutoff = wall_now - timedelta(days=self._stale_after_days)
        archive_cutoff = wall_now - timedelta(days=self._archive_after_days)
        counts = {"staled": 0, "archived": 0, "revived": 0, "skipped_raced": 0}

        for skill in candidates:
            # A never-used skill ages from ``created_at``; the guard,
            # however, is always the *raw* ``last_used_at`` snapshot
            # (which may be ``None``) -- that is the column a
            # concurrent ``mark_used`` would change.
            last_used = skill.last_used_at or skill.created_at

            if skill.state == "active" and last_used < stale_cutoff:
                if await self._transition_to_stale(skill, wall_now):
                    counts["staled"] += 1
                else:
                    counts["skipped_raced"] += 1
            elif skill.state == "stale":
                if last_used < archive_cutoff:
                    if await self._transition_to_archived(skill, wall_now):
                        counts["archived"] += 1
                    else:
                        counts["skipped_raced"] += 1
                # The agent used the skill again after a stale marker
                # but before this tick -- flip it back.
                elif last_used > stale_cutoff and await self._transition_revived(
                    skill, wall_now
                ):
                    counts["revived"] += 1
        logger.info(
            "skill_curator_tick_complete",
            scanned=len(candidates),
            **counts,
        )
        return counts

    async def _transition_to_stale(
        self, skill: SynthesizedSkill, wall_now: datetime
    ) -> bool:
        ok = await self._store.transition_state(
            skill.id,
            new_state="stale",
            reason="curator:idle_past_stale_window",
            guard_last_used_at=skill.last_used_at,
        )
        if not ok:
            return False
        await self._publish(
            SkillStaled(
                source=skill.agent_handle,
                agent_handle=skill.agent_handle,
                skill_id=str(skill.id),
                skill_name=skill.name,
                last_used_at=(
                    skill.last_used_at.isoformat() if skill.last_used_at else ""
                ),
                transitioned_at=wall_now.isoformat(),
            ),
            topic="crewlet.events.skill_staled",
        )
        return True

    async def _transition_to_archived(
        self, skill: SynthesizedSkill, wall_now: datetime
    ) -> bool:
        ok = await self._store.transition_state(
            skill.id,
            new_state="archived",
            reason="curator:idle_past_archive_window",
            guard_last_used_at=skill.last_used_at,
        )
        if not ok:
            return False
        await self._publish(
            SkillArchived(
                source=skill.agent_handle,
                agent_handle=skill.agent_handle,
                skill_id=str(skill.id),
                skill_name=skill.name,
                last_used_at=(
                    skill.last_used_at.isoformat() if skill.last_used_at else ""
                ),
                transitioned_at=wall_now.isoformat(),
            ),
            topic="crewlet.events.skill_archived",
        )
        return True

    async def _transition_revived(
        self, skill: SynthesizedSkill, wall_now: datetime
    ) -> bool:
        # Capture the prior state BEFORE the transition lands. The
        # store may mutate the row in-place (tests use a dict-backed
        # stub that does exactly that), so reading ``skill.state``
        # after ``transition_state`` would return the new state.
        # Revival is unguarded -- reviving a recently-used skill is
        # the safe direction and must never be blocked.
        prior_state = skill.state
        ok = await self._store.transition_state(
            skill.id, new_state="active", reason="curator:recent_use_after_stale"
        )
        if not ok:
            return False
        await self._publish(
            SkillRevived(
                source=skill.agent_handle,
                agent_handle=skill.agent_handle,
                skill_id=str(skill.id),
                skill_name=skill.name,
                prior_state=prior_state,
                transitioned_at=wall_now.isoformat(),
            ),
            topic="crewlet.events.skill_revived",
        )
        return True

    async def _publish(self, event: Any, *, topic: str) -> None:
        if self._event_queue is None:
            return
        try:
            await self._event_queue.publish(topic, event)
        except Exception:
            logger.exception("skill_curator_event_publish_failed", topic=topic)


__all__ = ["SkillCuratorWorker"]
