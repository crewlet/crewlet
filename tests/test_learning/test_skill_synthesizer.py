"""Tests for :class:`SkillSynthesizer`."""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any
from uuid import UUID, uuid4

from crewlet.learning.models import Episode, SynthesizedSkill
from crewlet.learning.skill_synthesizer import (
    NOOP_SENTINEL,
    SkillSynthesizer,
    jaccard,
)
from crewlet.org.models import Role
from crewlet.providers.llm.protocol import Completion


class _ScriptedProvider:
    def __init__(self, completions: list[Completion]) -> None:
        self._completions = list(completions)
        self.calls: list[dict[str, Any]] = []
        self.model = "scripted"

    async def complete(
        self,
        messages,
        tools=None,
        temperature: float = 0.7,
        max_tokens: int | None = None,
        tool_choice=None,
    ):
        self.calls.append({"messages": messages})
        if self._completions:
            return self._completions.pop(0)
        return Completion(content="NOOP")


class _StoreStub:
    def __init__(
        self,
        *,
        count: int = 0,
        existing_sequences: list[list[str]] | None = None,
        fetch_result: SynthesizedSkill | None = None,
    ) -> None:
        self._count = count
        self._existing_sequences = existing_sequences or []
        self._fetch_result = fetch_result
        self.inserts: list[SynthesizedSkill] = []
        self.fetches: list[dict[str, str]] = []

    async def insert(self, skill: SynthesizedSkill) -> SynthesizedSkill:
        self.inserts.append(skill)
        return skill

    async def fetch(self, *, agent_handle: str, name: str) -> SynthesizedSkill | None:
        self.fetches.append({"agent_handle": agent_handle, "name": name})
        return self._fetch_result

    async def list_for_agent(self, agent_handle: str):  # pragma: no cover
        return []

    async def count_for_agent(self, agent_handle: str) -> int:
        return self._count

    async def existing_tool_sequences(self, agent_handle: str):
        return list(self._existing_sequences)


def _mk_role() -> Role:
    return Role(name="Engineer", handle="eng", llm="cheap", llm_auxiliary="cheap")


def _mk_episode(
    *, tool_sequence: list[str] | None = None, outcome: str = "done"
) -> Episode:
    return Episode(
        id=uuid4(),
        agent_handle="alice",
        agent_role="Engineer",
        task_id="t",
        turn_id=uuid4(),
        started_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        ended_at=datetime(2026, 4, 23, 9, 5, tzinfo=UTC),
        plan_summary="Do the thing",
        task_summary="Help with a thing",
        tool_sequence=tool_sequence
        or ["slack_message", "jira_comment", "query_knowledge"],
        skills_used=[],
        review_outcome=outcome,  # type: ignore[arg-type]
        duration_ms=60_000,
    )


def test_jaccard_basic() -> None:
    assert jaccard(["a", "b"], ["a", "b"]) == 1.0
    assert jaccard(["a", "b"], ["b", "c"]) == 1 / 3
    assert jaccard([], ["a"]) == 0.0
    assert jaccard(["a"], []) == 0.0


async def test_noop_response_skips_insert() -> None:
    provider = _ScriptedProvider([Completion(content="NOOP")])
    store = _StoreStub()
    synth = SkillSynthesizer(llm_providers={"cheap": provider}, store=store)
    result = await synth.synthesize(
        role=_mk_role(),
        agent_handle="alice",
        source_episodes=[_mk_episode()],
    )
    assert result is None
    assert store.inserts == []


async def test_synthesize_stamps_consolidated_into_skill_id() -> None:
    """After a successful skill insert, source episodes get
    ``consolidated_into_skill_id`` stamped via the episode store so
    the lifecycle worker can drop them after grace."""

    class _EpisodeStoreStub:
        def __init__(self) -> None:
            self.calls: list[dict[str, Any]] = []

        async def mark_consolidated(
            self, *, episode_ids: list[str], skill_id: str
        ) -> int:
            self.calls.append({"episode_ids": list(episode_ids), "skill_id": skill_id})
            return len(episode_ids)

    provider = _ScriptedProvider(
        [
            Completion(
                content=json.dumps(
                    {
                        "name": "close-the-loop",
                        "description": "Close the loop after ship.",
                        "body": "1. Ship\n2. Post update",
                    }
                )
            )
        ]
    )
    store = _StoreStub()
    episode_store = _EpisodeStoreStub()
    synth = SkillSynthesizer(
        llm_providers={"cheap": provider},
        store=store,
        episode_store=episode_store,
    )
    sources = [_mk_episode(), _mk_episode()]
    result = await synth.synthesize(
        role=_mk_role(), agent_handle="alice", source_episodes=sources
    )
    assert result is not None
    assert len(episode_store.calls) == 1
    call = episode_store.calls[0]
    assert call["skill_id"] == str(result.id)
    # All source episode ids stamped.
    assert call["episode_ids"] == [str(s.id) for s in sources]


async def test_synthesize_does_not_break_when_episode_store_missing() -> None:
    """Synthesis must still work when no episode store is wired
    (in-memory tests, engines without episodic memory)."""
    provider = _ScriptedProvider(
        [
            Completion(
                content=json.dumps(
                    {
                        "name": "noop-shape",
                        "description": "x",
                        "body": "y",
                    }
                )
            )
        ]
    )
    store = _StoreStub()
    synth = SkillSynthesizer(
        llm_providers={"cheap": provider},
        store=store,
        episode_store=None,
    )
    result = await synth.synthesize(
        role=_mk_role(), agent_handle="alice", source_episodes=[_mk_episode()]
    )
    assert result is not None  # didn't blow up


async def test_valid_json_proposal_inserts_skill() -> None:
    provider = _ScriptedProvider(
        [
            Completion(
                content=json.dumps(
                    {
                        "name": "close-the-loop",
                        "description": "Close the loop after ship.",
                        "body": "1. Ship\n2. Post update",
                    }
                )
            )
        ]
    )
    store = _StoreStub()
    synth = SkillSynthesizer(llm_providers={"cheap": provider}, store=store)
    result = await synth.synthesize(
        role=_mk_role(),
        agent_handle="alice",
        source_episodes=[_mk_episode()],
    )
    assert result is not None
    assert result.name == "close-the-loop"
    assert len(store.inserts) == 1
    # System prompt is the strict JSON one.
    sys_msg = provider.calls[0]["messages"][0]
    assert sys_msg.role == "system"
    assert NOOP_SENTINEL in sys_msg.content
    assert "skill-drafting assistant" in sys_msg.content


async def test_cap_reached_short_circuits_llm() -> None:
    provider = _ScriptedProvider([])
    store = _StoreStub(count=5)
    synth = SkillSynthesizer(
        llm_providers={"cheap": provider},
        store=store,
        max_skills_per_agent=5,
    )
    result = await synth.synthesize(
        role=_mk_role(),
        agent_handle="alice",
        source_episodes=[_mk_episode()],
    )
    assert result is None
    # LLM never called.
    assert provider.calls == []


async def test_name_collision_against_existing_registered_rejected() -> None:
    provider = _ScriptedProvider(
        [
            Completion(
                content=json.dumps(
                    {
                        "name": "code-review",
                        "description": "Review code thoroughly.",
                        "body": "Step 1...",
                    }
                )
            )
        ]
    )
    store = _StoreStub()
    synth = SkillSynthesizer(llm_providers={"cheap": provider}, store=store)
    result = await synth.synthesize(
        role=_mk_role(),
        agent_handle="alice",
        source_episodes=[_mk_episode()],
        existing_skill_names=["code-review", "testing"],
    )
    assert result is None
    assert store.inserts == []


async def test_name_collision_against_existing_synthesized_rejected() -> None:
    provider = _ScriptedProvider(
        [
            Completion(
                content=json.dumps(
                    {
                        "name": "already-here",
                        "description": "desc",
                        "body": "body",
                    }
                )
            )
        ]
    )
    existing = SynthesizedSkill(
        id=uuid4(),
        agent_handle="alice",
        name="already-here",
        description="existing",
        content="existing",
        created_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        updated_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    )
    store = _StoreStub(fetch_result=existing)
    synth = SkillSynthesizer(llm_providers={"cheap": provider}, store=store)
    result = await synth.synthesize(
        role=_mk_role(),
        agent_handle="alice",
        source_episodes=[_mk_episode()],
    )
    assert result is None
    assert store.inserts == []


async def test_duplicate_tool_sequence_rejected_before_llm() -> None:
    provider = _ScriptedProvider([])
    store = _StoreStub(
        existing_sequences=[["slack_message", "jira_comment", "query_knowledge"]]
    )
    synth = SkillSynthesizer(
        llm_providers={"cheap": provider},
        store=store,
        duplicate_jaccard_threshold=0.7,
    )
    result = await synth.synthesize(
        role=_mk_role(),
        agent_handle="alice",
        source_episodes=[_mk_episode()],
    )
    assert result is None
    assert provider.calls == []


async def test_bad_name_rejected() -> None:
    # Synthesizer lowercases the LLM output before validation, so we
    # need a name that fails the pattern regardless of case: must
    # start and end with alphanumeric.  A leading hyphen always
    # fails even after normalisation.
    provider = _ScriptedProvider(
        [
            Completion(
                content=json.dumps(
                    {
                        "name": "-bad-name",
                        "description": "desc",
                        "body": "body",
                    }
                )
            )
        ]
    )
    store = _StoreStub()
    synth = SkillSynthesizer(llm_providers={"cheap": provider}, store=store)
    result = await synth.synthesize(
        role=_mk_role(),
        agent_handle="alice",
        source_episodes=[_mk_episode()],
    )
    assert result is None
    assert store.inserts == []


async def test_llm_exception_returns_none() -> None:
    class _Boom:
        model = "boom"

        async def complete(self, *args, **kwargs):
            raise RuntimeError("provider down")

    store = _StoreStub()
    synth = SkillSynthesizer(llm_providers={"cheap": _Boom()}, store=store)
    result = await synth.synthesize(
        role=_mk_role(),
        agent_handle="alice",
        source_episodes=[_mk_episode()],
    )
    assert result is None
    assert store.inserts == []


async def test_empty_source_episodes_returns_none() -> None:
    synth = SkillSynthesizer(
        llm_providers={"cheap": _ScriptedProvider([])}, store=_StoreStub()
    )
    result = await synth.synthesize(
        role=_mk_role(),
        agent_handle="alice",
        source_episodes=[],
    )
    assert result is None


async def test_source_episode_ids_recorded_on_insert() -> None:
    provider = _ScriptedProvider(
        [
            Completion(
                content=json.dumps(
                    {
                        "name": "cluster-skill",
                        "description": "Clustered skill.",
                        "body": "body",
                    }
                )
            )
        ]
    )
    store = _StoreStub()
    synth = SkillSynthesizer(llm_providers={"cheap": provider}, store=store)
    eps = [_mk_episode(), _mk_episode(), _mk_episode()]
    await synth.synthesize(
        role=_mk_role(),
        agent_handle="alice",
        source_episodes=eps,
        trigger="clustered",
    )
    inserted = store.inserts[0]
    assert set(inserted.source_episode_ids) == {ep.id for ep in eps}
    assert all(isinstance(x, UUID) for x in inserted.source_episode_ids)
