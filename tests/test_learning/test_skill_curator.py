"""Tests for ``SkillCuratorWorker``.

Covers the state-machine transitions (active → stale → archived,
stale → active via revival), the pinned-skill exemption, the
recently-used safety window that mitigates the read-then-archive race
with the Plan-phase prefetch, and the ``_use_skill`` archived-rejection
behaviour.
"""

from __future__ import annotations

from datetime import UTC, datetime, timedelta
from typing import Any
from uuid import UUID, uuid4

from crewlet.events.types import SkillArchived, SkillRevived, SkillStaled
from crewlet.learning.models import SynthesizedSkill
from crewlet.learning.skill_curator import SkillCuratorWorker
from crewlet.tools.builtin import _use_skill
from crewlet.tools.protocol import AgentContext

# --- Fakes --------------------------------------------------------------


class _QueueStub:
    def __init__(self) -> None:
        self.published: list[tuple[str, Any]] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


_NO_GUARD: object = object()


class _StoreStub:
    """Minimal stub of the skill store the curator uses.

    ``transition_state`` mirrors the real store's optimistic-
    concurrency guard: when ``guard_last_used_at`` is supplied, the
    transition only applies if the stub row's ``last_used_at`` still
    matches the snapshot. ``raced`` lets a test simulate a concurrent
    ``mark_used`` having bumped ``last_used_at`` after the curator's
    candidate SELECT.
    """

    def __init__(
        self, skills: list[SynthesizedSkill], *, raced_ids: set[UUID] | None = None
    ) -> None:
        self.skills = {s.id: s for s in skills}
        self.transitions: list[tuple[UUID, str, str]] = []
        # Skill ids whose ``last_used_at`` we pretend was bumped by a
        # concurrent ``mark_used`` after the candidate snapshot.
        self._raced_ids = raced_ids or set()

    async def list_curator_candidates(
        self, agent_handle: str = ""
    ) -> list[SynthesizedSkill]:
        return [
            s for s in self.skills.values() if not s.pinned and s.state != "archived"
        ]

    async def transition_state(
        self,
        skill_id: UUID,
        *,
        new_state: str,
        reason: str = "",
        guard_last_used_at: object = _NO_GUARD,
    ) -> bool:
        # Guarded transition that lost the race -> no-op, return False.
        if guard_last_used_at is not _NO_GUARD and skill_id in self._raced_ids:
            return False
        self.transitions.append((skill_id, new_state, reason))
        if skill_id in self.skills:
            self.skills[skill_id].state = new_state
            return True
        return False


def _skill(
    *,
    name: str,
    state: str = "active",
    pinned: bool = False,
    last_used_days_ago: float | None = None,
    created_days_ago: float = 5.0,
) -> SynthesizedSkill:
    now = datetime.now(UTC)
    last_used = (
        now - timedelta(days=last_used_days_ago)
        if last_used_days_ago is not None
        else None
    )
    return SynthesizedSkill(
        id=uuid4(),
        agent_handle="alice",
        name=name,
        description="d",
        content="body",
        version=1,
        created_at=now - timedelta(days=created_days_ago),
        updated_at=now - timedelta(days=created_days_ago),
        use_count=0 if last_used is None else 1,
        last_used_at=last_used,
        state=state,
        pinned=pinned,
    )


# --- Curator transitions ----------------------------------------------


async def test_curator_marks_active_skill_stale_after_idle_window() -> None:
    """An active skill unused for ``stale_after_days`` flips to
    ``stale`` and a ``SkillStaled`` event is published."""
    skill = _skill(
        name="old-active",
        state="active",
        last_used_days_ago=40,
        created_days_ago=120,
    )
    store = _StoreStub([skill])
    queue = _QueueStub()
    worker = SkillCuratorWorker(
        store=store,
        event_queue=queue,
        stale_after_days=30,
        archive_after_days=90,
    )
    counts = await worker.tick_once()
    assert counts["staled"] == 1
    assert counts["archived"] == 0
    assert store.transitions == [(skill.id, "stale", "curator:idle_past_stale_window")]
    staled = [e for _t, e in queue.published if isinstance(e, SkillStaled)]
    assert staled and staled[-1].skill_name == "old-active"


async def test_curator_archives_stale_skill_after_archive_window() -> None:
    """A stale skill unused for ``archive_after_days`` flips to
    ``archived`` and ``SkillArchived`` fires."""
    skill = _skill(
        name="dead",
        state="stale",
        last_used_days_ago=100,
        created_days_ago=200,
    )
    store = _StoreStub([skill])
    queue = _QueueStub()
    worker = SkillCuratorWorker(
        store=store,
        event_queue=queue,
        stale_after_days=30,
        archive_after_days=90,
    )
    counts = await worker.tick_once()
    assert counts["archived"] == 1
    archived = [e for _t, e in queue.published if isinstance(e, SkillArchived)]
    assert archived and archived[-1].skill_name == "dead"


async def test_curator_revives_stale_skill_after_recent_use() -> None:
    """A stale-marked skill whose ``last_used_at`` has moved forward
    past the stale cutoff transitions back to ``active``."""
    skill = _skill(
        name="back-from-dead",
        state="stale",
        last_used_days_ago=2,
        created_days_ago=120,
    )
    store = _StoreStub([skill])
    queue = _QueueStub()
    worker = SkillCuratorWorker(
        store=store,
        event_queue=queue,
        stale_after_days=30,
        archive_after_days=90,
    )
    counts = await worker.tick_once()
    assert counts["revived"] == 1
    revived = [e for _t, e in queue.published if isinstance(e, SkillRevived)]
    assert revived
    assert revived[-1].prior_state == "stale"


async def test_curator_exempts_pinned_skills() -> None:
    """A pinned skill -- even ancient and unused -- is never
    transitioned by the curator. The pinned flag is the operator
    override."""
    pinned = _skill(
        name="pinned",
        state="active",
        pinned=True,
        last_used_days_ago=365,
        created_days_ago=400,
    )
    store = _StoreStub([pinned])
    worker = SkillCuratorWorker(
        store=store,
        stale_after_days=30,
        archive_after_days=90,
    )
    counts = await worker.tick_once()
    # Pinned skills are excluded from ``list_curator_candidates``
    # outright -- they should never appear in the worker's scan.
    assert counts == {"staled": 0, "archived": 0, "revived": 0, "skipped_raced": 0}
    assert store.transitions == []


async def test_curator_skips_skill_raced_by_concurrent_use() -> None:
    """The read-then-archive race fix: a skill formally past the
    archive cutoff that a concurrent ``mark_used`` touched between
    the candidate SELECT and the guarded UPDATE is NOT archived --
    ``transition_state``'s optimistic guard no-ops, the curator
    counts it as ``skipped_raced``, and no event fires."""
    skill = _skill(
        name="raced-out",
        state="stale",
        last_used_days_ago=100,
        created_days_ago=200,
    )
    # Simulate ``mark_used`` having bumped this skill's last_used_at
    # after the curator's snapshot: the guarded UPDATE matches 0 rows.
    store = _StoreStub([skill], raced_ids={skill.id})
    queue = _QueueStub()
    worker = SkillCuratorWorker(
        store=store,
        event_queue=queue,
        stale_after_days=30,
        archive_after_days=90,
    )
    counts = await worker.tick_once()
    assert counts["archived"] == 0
    assert counts["skipped_raced"] == 1
    # No transition recorded, no event published.
    assert store.transitions == []
    assert not [e for _t, e in queue.published if isinstance(e, SkillArchived)]


async def test_curator_revival_is_unguarded() -> None:
    """Revival (``stale → active``) must NOT carry the optimistic
    guard -- reviving a recently-used skill is the safe direction.
    Even with the skill marked 'raced', revival still applies."""
    skill = _skill(
        name="revive-me",
        state="stale",
        last_used_days_ago=2,
        created_days_ago=120,
    )
    # ``raced_ids`` only blocks *guarded* transitions; revival passes
    # no guard, so it applies regardless.
    store = _StoreStub([skill], raced_ids={skill.id})
    queue = _QueueStub()
    worker = SkillCuratorWorker(
        store=store,
        event_queue=queue,
        stale_after_days=30,
        archive_after_days=90,
    )
    counts = await worker.tick_once()
    assert counts["revived"] == 1
    assert counts["skipped_raced"] == 0


# --- _use_skill archived rejection -------------------------------------


async def test_use_skill_refuses_archived_skill() -> None:
    """``_use_skill`` returns a clean error when the resolved skill
    has ``state='archived'`` -- archived skills are not loadable."""
    archived = _skill(
        name="ghost",
        state="archived",
        last_used_days_ago=200,
    )

    class _ResolvingStore:
        # ``fetch`` would normally filter archived rows out, but
        # we simulate the edge case (a future caller might pass
        # ``include_archived=True``) by returning the row directly.
        async def fetch(
            self,
            *,
            agent_handle: str,
            name: str,
            include_archived: bool = False,
        ) -> SynthesizedSkill | None:
            return archived

        async def mark_used(self, *_args, **_kwargs) -> None:
            raise AssertionError("mark_used must not fire on archived skills")

    queue = _QueueStub()
    ctx = AgentContext(
        agent_id="a",
        agent_handle="alice",
        role="Engineer",
        event_queue=queue,
        synthesized_skill_store=_ResolvingStore(),
    )
    result = await _use_skill({"skill_name": "ghost"}, ctx)
    assert not result.success
    assert "archived" in (result.error or "")


async def test_use_skill_surfaces_stale_marker_on_load() -> None:
    """A stale skill loads but carries a stale-marker comment so the
    agent knows it's aging."""
    stale = _skill(name="aging", state="stale")

    class _Store:
        async def fetch(
            self,
            *,
            agent_handle: str,
            name: str,
            include_archived: bool = False,
        ) -> SynthesizedSkill | None:
            return stale

        async def mark_used(
            self,
            skill_id: UUID,
            *,
            event_queue: Any = None,
            skill_name: str = "",
            agent_handle: str = "",
        ) -> None:
            return None

    ctx = AgentContext(
        agent_id="a",
        agent_handle="alice",
        role="Engineer",
        event_queue=_QueueStub(),
        synthesized_skill_store=_Store(),
    )
    result = await _use_skill({"skill_name": "aging"}, ctx)
    assert result.success
    assert "stale" in result.output.lower()
