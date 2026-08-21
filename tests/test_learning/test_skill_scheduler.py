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


def _mk_org(handles: tuple[str, ...] = ("alice",)) -> Organization:
    """A company whose seats are the ones the test drives.

    The pass reads its work list from the ORG, not from this node's
    agent pool — it is a fleet-wide singleton, so the pool holds only
    the seats this node happens to have claimed.
    """
    roles = [
        Role(name="Engineer" if i == 0 else f"Engineer{i}", handle=handle)
        for i, handle in enumerate(handles)
    ]
    unit = OrgUnit(name="Eng", type="team", lead="Engineer", roles=roles)
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
        organization=_mk_org(("alice", "bob")),
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


# --- clustering is a fleet-wide singleton ---------------------------------


def _gated(claim_duty: Any) -> SkillClusteringScheduler:
    return SkillClusteringScheduler(
        synthesizer=None,  # type: ignore[arg-type]
        episode_store=None,  # type: ignore[arg-type]
        agent_pool=_AgentPoolStub([]),
        organization=_mk_org(),
        claim_duty=claim_duty,
    )


async def test_a_node_without_the_duty_does_not_cluster() -> None:
    """Synthesis reads every agent's episodes and WRITES synthesized
    skills, so running it on every node means N nodes clustering the
    same episodes into N sets of near-identical auto-drafted pages —
    duplicated output and N times the LLM spend for one pass's value."""
    assert await _gated(_never)._may_tick() is False


async def test_the_duty_holder_clusters() -> None:
    assert await _gated(_always)._may_tick() is True


async def test_no_placement_host_clusters_as_before() -> None:
    """Single node: there is no fleet to be a singleton within."""
    assert await _gated(None)._may_tick() is True


async def test_a_duty_claim_that_raises_skips_the_pass() -> None:
    async def _boom() -> bool:
        raise RuntimeError("lease store unreachable")

    assert await _gated(_boom)._may_tick() is False


async def _always() -> bool:
    return True


async def _never() -> bool:
    return False


async def test_the_pass_covers_seats_this_node_does_not_run() -> None:
    """The tick is a fleet-wide singleton.

    It runs on whichever node holds the `skill-clustering` duty, so
    reading the local agent pool meant that node clustered only the
    seats IT had claimed — on a three-node fleet, six of nine agents
    never got a scheduled pass, and every log line read healthy.
    """
    eps = [
        _mk_episode(tool_sequence=["a", "b", "c"], agent_handle="bob", minutes=i)
        for i in range(3)
    ]

    class _BobStore(_EpisodeStoreStub):
        async def list_recent_by_outcome(self, **kwargs):
            return list(eps) if kwargs.get("agent_handle") == "bob" else []

    synth = _RecordingSynthesizer()
    scheduler = SkillClusteringScheduler(
        synthesizer=synth,  # type: ignore[arg-type]
        episode_store=_BobStore([]),  # type: ignore[arg-type]
        # This node runs only alice. bob's seat is held by a peer.
        agent_pool=_AgentPoolStub([_AgentStub("alice")]),
        organization=_mk_org(("alice", "bob")),
        cluster_min_size=3,
    )

    made = await scheduler.tick_once()

    assert made == 1
    assert [c["agent_handle"] for c in synth.calls] == ["bob"]


async def test_a_workers_only_node_still_covers_the_company() -> None:
    """The documented split-role shape, and the worst case.

    A node started with `roles: [workers]` runs no seats at all, so its
    pool is empty — and it is exactly the node most likely to hold the
    duty, which means NOBODY got a scheduled skill pass.
    """
    eps = [
        _mk_episode(tool_sequence=["a", "b", "c"], agent_handle="alice", minutes=i)
        for i in range(3)
    ]

    class _AliceStore(_EpisodeStoreStub):
        async def list_recent_by_outcome(self, **kwargs):
            return list(eps) if kwargs.get("agent_handle") == "alice" else []

    synth = _RecordingSynthesizer()
    scheduler = SkillClusteringScheduler(
        synthesizer=synth,  # type: ignore[arg-type]
        episode_store=_AliceStore([]),  # type: ignore[arg-type]
        agent_pool=_AgentPoolStub([]),  # ingress/workers node: no seats
        organization=_mk_org(("alice",)),
        cluster_min_size=3,
    )

    assert await scheduler.tick_once() == 1
    assert [c["agent_handle"] for c in synth.calls] == ["alice"]
