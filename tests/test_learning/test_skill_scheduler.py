"""Tests for :class:`SkillClusteringScheduler`."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from crewlet.learning.models import Episode, SynthesizedSkill
from crewlet.learning.skill_scheduler import (
    SkillClusteringScheduler,
    _cluster_by_jaccard,
)
from crewlet.org.models import Organization, OrgUnit, Role


class _AgentStub:
    def __init__(self, handle: str, role_name: str = "Engineer") -> None:
        self.handle = handle
        self.role_name = role_name
        role = Role(name=role_name, handle=handle)
        self.definition = type("_D", (), {"role": role})()


class _AgentPoolStub:
    def __init__(self, agents: list[_AgentStub]) -> None:
        self.agents = agents


class _EpisodeStoreStub:
    def __init__(self, episodes: list[Episode]) -> None:
        self._episodes = episodes
        self.calls: list[dict[str, Any]] = []

    async def list_recent_by_outcome(self, **kwargs) -> list[Episode]:
        self.calls.append(kwargs)
        return list(self._episodes)

    async def fetch_by_turn_id(self, turn_id: str):  # pragma: no cover
        return None

    async def write(self, episode: Episode) -> None:  # pragma: no cover
        pass

    async def query(self, **kwargs):  # pragma: no cover
        return []


class _RecordingSynthesizer:
    def __init__(self, *, make: bool = True) -> None:
        self._make = make
        self.calls: list[dict[str, Any]] = []

    async def synthesize(self, **kwargs) -> SynthesizedSkill | None:
        self.calls.append(kwargs)
        if not self._make:
            return None
        return SynthesizedSkill(
            id=uuid4(),
            agent_handle=kwargs["agent_handle"],
            name=f"synth-{len(self.calls)}",
            description="desc",
            content="body",
            created_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
            updated_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        )


def _mk_episode(
    *, tool_sequence: list[str], agent_handle: str = "alice", minutes: int = 0
) -> Episode:
    ts = datetime(2026, 4, 23, 9, minutes, tzinfo=UTC)
    return Episode(
        id=uuid4(),
        agent_handle=agent_handle,
        agent_role="Engineer",
        task_id="t",
        turn_id=uuid4(),
        started_at=ts,
        ended_at=ts,
        plan_summary="p",
        task_summary="t",
        tool_sequence=tool_sequence,
        skills_used=[],
        review_outcome="done",
        duration_ms=60_000,
    )


def _mk_org() -> Organization:
    role = Role(name="Engineer", handle="eng")
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=[role])
    return Organization(name="Acme", units=[unit])


def test_cluster_groups_similar_sequences() -> None:
    eps = [
        _mk_episode(tool_sequence=["a", "b", "c"]),
        _mk_episode(tool_sequence=["a", "b", "c"]),
        _mk_episode(tool_sequence=["x", "y", "z"]),
        _mk_episode(tool_sequence=["a", "b", "c", "d"]),
    ]
    clusters = _cluster_by_jaccard(eps, threshold=0.6)
    # First cluster catches the two identical + the near-identical one.
    assert len(clusters) == 2
    sizes = sorted(len(c) for c in clusters)
    assert sizes == [1, 3]


def test_cluster_ignores_empty_sequences() -> None:
    eps = [
        _mk_episode(tool_sequence=[]),
        _mk_episode(tool_sequence=["a", "b"]),
    ]
    clusters = _cluster_by_jaccard(eps, threshold=0.6)
    assert len(clusters) == 1


async def test_tick_once_synthesises_one_skill_per_qualifying_cluster() -> None:
    # 3 similar + 1 outlier → one cluster qualifies, outlier does not
    # (below min_size).
    eps = [
        _mk_episode(tool_sequence=["a", "b", "c"], minutes=1),
        _mk_episode(tool_sequence=["a", "b", "c"], minutes=2),
        _mk_episode(tool_sequence=["a", "b", "c"], minutes=3),
        _mk_episode(tool_sequence=["x", "y"], minutes=4),
    ]
    store = _EpisodeStoreStub(eps)
    synth = _RecordingSynthesizer()
    scheduler = SkillClusteringScheduler(
        synthesizer=synth,  # type: ignore[arg-type]
        episode_store=store,  # type: ignore[arg-type]
        agent_pool=_AgentPoolStub([_AgentStub("alice")]),
        organization=_mk_org(),
        cluster_min_size=3,
        cluster_jaccard_threshold=0.6,
    )
    made = await scheduler.tick_once()
    assert made == 1
    assert len(synth.calls) == 1
    assert synth.calls[0]["trigger"] == "clustered"
    assert len(synth.calls[0]["source_episodes"]) == 3


async def test_tick_once_skips_agent_with_too_few_episodes() -> None:
    store = _EpisodeStoreStub([_mk_episode(tool_sequence=["a", "b"], minutes=1)])
    synth = _RecordingSynthesizer()
    scheduler = SkillClusteringScheduler(
        synthesizer=synth,  # type: ignore[arg-type]
        episode_store=store,  # type: ignore[arg-type]
        agent_pool=_AgentPoolStub([_AgentStub("alice")]),
        organization=_mk_org(),
        cluster_min_size=3,
    )
    made = await scheduler.tick_once()
    assert made == 0
    assert synth.calls == []


async def test_tick_once_handles_multiple_agents_independently() -> None:
    eps = [
        _mk_episode(tool_sequence=["a", "b", "c"], agent_handle="alice", minutes=i)
        for i in range(3)
    ]

    class _MultiStore(_EpisodeStoreStub):
        async def list_recent_by_outcome(self, **kwargs):
            if kwargs.get("agent_handle") == "alice":
                return list(eps)
            return []

    store = _MultiStore([])
    synth = _RecordingSynthesizer()
    scheduler = SkillClusteringScheduler(
        synthesizer=synth,  # type: ignore[arg-type]
        episode_store=store,  # type: ignore[arg-type]
        agent_pool=_AgentPoolStub([_AgentStub("alice"), _AgentStub("bob")]),
        organization=_mk_org(),
        cluster_min_size=3,
    )
    made = await scheduler.tick_once()
    assert made == 1
    alice_calls = [c for c in synth.calls if c["agent_handle"] == "alice"]
    bob_calls = [c for c in synth.calls if c["agent_handle"] == "bob"]
    assert len(alice_calls) == 1
    assert bob_calls == []


async def test_tick_once_swallows_agent_exception() -> None:
    class _BoomStore(_EpisodeStoreStub):
        async def list_recent_by_outcome(self, **kwargs):
            raise RuntimeError("db down")

    scheduler = SkillClusteringScheduler(
        synthesizer=_RecordingSynthesizer(),  # type: ignore[arg-type]
        episode_store=_BoomStore([]),  # type: ignore[arg-type]
        agent_pool=_AgentPoolStub([_AgentStub("alice")]),
        organization=_mk_org(),
    )
    # Must not raise.
    made = await scheduler.tick_once()
    assert made == 0


async def test_start_and_stop_cleanly_cancel_task() -> None:
    scheduler = SkillClusteringScheduler(
        synthesizer=_RecordingSynthesizer(),  # type: ignore[arg-type]
        episode_store=_EpisodeStoreStub([]),  # type: ignore[arg-type]
        agent_pool=_AgentPoolStub([]),
        organization=_mk_org(),
        interval_seconds=3600,
    )
    await scheduler.start()
    assert scheduler._running is True
    await scheduler.stop()
    assert scheduler._running is False
    assert scheduler._task is None
