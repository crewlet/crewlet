"""Tests for :class:`SkillRefiner`."""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from crewlet.learning.models import SynthesizedSkill
from crewlet.learning.skill_refiner import (
    _REFINER_SYSTEM_PROMPT,
    NOOP_SENTINEL,
    SkillRefiner,
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
    def __init__(self, skills: dict[str, SynthesizedSkill] | None = None) -> None:
        self._skills = skills or {}
        self.updates: list[dict[str, Any]] = []
        self.fetches: list[dict[str, str]] = []

    async def insert(
        self, skill: SynthesizedSkill
    ) -> SynthesizedSkill:  # pragma: no cover
        return skill

    async def fetch(self, *, agent_handle: str, name: str) -> SynthesizedSkill | None:
        self.fetches.append({"agent_handle": agent_handle, "name": name})
        return self._skills.get(name)

    async def list_for_agent(self, agent_handle: str):
        return list(self._skills.values())

    async def count_for_agent(self, agent_handle: str) -> int:  # pragma: no cover
        return len(self._skills)

    async def existing_tool_sequences(self, agent_handle: str):  # pragma: no cover
        return []

    async def update(
        self,
        skill: SynthesizedSkill,
        *,
        refinement_kind: str,
        refinement_note: str = "",
        max_versions_kept: int = 10,
    ) -> SynthesizedSkill:
        self.updates.append(
            {
                "skill": skill,
                "refinement_kind": refinement_kind,
                "refinement_note": refinement_note,
                "max_versions_kept": max_versions_kept,
            }
        )
        bumped = skill.model_copy(update={"version": skill.version + 1})
        self._skills[skill.name] = bumped
        return bumped

    async def list_for_agent_handles(
        self, agent_handles: list[str]
    ):  # pragma: no cover
        return []

    async def list_versions(self, *a, **k):  # pragma: no cover
        return []

    async def rollback_to_version(self, **k):  # pragma: no cover
        return None


def _mk_role() -> Role:
    return Role(name="Engineer", handle="eng", llm="cheap", llm_auxiliary="cheap")


def _mk_skill(*, name: str = "close-the-loop") -> SynthesizedSkill:
    return SynthesizedSkill(
        id=uuid4(),
        agent_handle="alice",
        name=name,
        description="Close the loop after ship.",
        content="## Close the loop\n1. Ship.\n2. Post update.",
        tool_sequence=["slack_message"],
        version=1,
        created_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        updated_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    )


async def test_success_path_appends_observed_in_practice_note() -> None:
    provider = _ScriptedProvider(
        [
            Completion(
                content=json.dumps({"note": "Worked cleanly when the PR was small."})
            )
        ]
    )
    store = _StoreStub({"close-the-loop": _mk_skill()})
    refiner = SkillRefiner(llm_providers={"cheap": provider}, store=store)

    result = await refiner.refine_from_turn(
        role=_mk_role(),
        agent_handle="alice",
        skills_used=["close-the-loop"],
        review_outcome="done",
        task_summary="ship PR-123",
        plan_summary="close the loop",
        turn_id="t1",
    )

    assert result is not None
    assert len(store.updates) == 1
    update = store.updates[0]
    assert update["refinement_kind"] == "observed_in_practice"
    assert "Observed in practice" in update["skill"].content
    assert "Worked cleanly" in update["skill"].content


async def test_failure_path_appends_counter_example() -> None:
    provider = _ScriptedProvider(
        [Completion(content=json.dumps({"note": "Breaks when Slack webhook is down."}))]
    )
    store = _StoreStub({"close-the-loop": _mk_skill()})
    refiner = SkillRefiner(llm_providers={"cheap": provider}, store=store)

    result = await refiner.refine_from_turn(
        role=_mk_role(),
        agent_handle="alice",
        skills_used=["close-the-loop"],
        review_outcome="escalate",
        task_summary="ship PR-123",
        plan_summary="close the loop",
        turn_id="t2",
    )

    assert result is not None
    assert store.updates[0]["refinement_kind"] == "counter_example"
    assert "Counter-example" in store.updates[0]["skill"].content


async def test_noop_response_skips_update() -> None:
    provider = _ScriptedProvider([Completion(content="NOOP")])
    store = _StoreStub({"close-the-loop": _mk_skill()})
    refiner = SkillRefiner(llm_providers={"cheap": provider}, store=store)

    result = await refiner.refine_from_turn(
        role=_mk_role(),
        agent_handle="alice",
        skills_used=["close-the-loop"],
        review_outcome="done",
        task_summary="x",
        plan_summary="x",
        turn_id="t",
    )

    assert result is None
    assert store.updates == []
    # System prompt is the refiner's strict prompt.
    sys_msg = provider.calls[0]["messages"][0]
    assert sys_msg.role == "system"
    assert NOOP_SENTINEL in _REFINER_SYSTEM_PROMPT
    assert "skill-refinement assistant" in _REFINER_SYSTEM_PROMPT


async def test_body_cap_reached_skips_refinement() -> None:
    provider = _ScriptedProvider([Completion(content=json.dumps({"note": "x"}))])
    big_skill = _mk_skill()
    big_skill = big_skill.model_copy(update={"content": "x" * 5000})
    store = _StoreStub({"close-the-loop": big_skill})
    refiner = SkillRefiner(
        llm_providers={"cheap": provider},
        store=store,
        max_body_chars=1000,  # far below len("x"*5000)
    )

    result = await refiner.refine_from_turn(
        role=_mk_role(),
        agent_handle="alice",
        skills_used=["close-the-loop"],
        review_outcome="done",
        task_summary="",
        plan_summary="",
        turn_id="t",
    )

    assert result is None
    assert provider.calls == []  # LLM never called
    assert store.updates == []


async def test_picks_most_recently_used_when_multiple_skills() -> None:
    provider = _ScriptedProvider(
        [Completion(content=json.dumps({"note": "observation"}))]
    )
    store = _StoreStub(
        {
            "first-skill": _mk_skill(name="first-skill"),
            "last-skill": _mk_skill(name="last-skill"),
        }
    )
    refiner = SkillRefiner(llm_providers={"cheap": provider}, store=store)

    await refiner.refine_from_turn(
        role=_mk_role(),
        agent_handle="alice",
        skills_used=["first-skill", "last-skill"],
        review_outcome="done",
        task_summary="",
        plan_summary="",
        turn_id="t",
    )

    assert len(store.updates) == 1
    assert store.updates[0]["skill"].name == "last-skill"


async def test_auto_refine_on_success_disabled_skips_successful_turns() -> None:
    provider = _ScriptedProvider([])
    store = _StoreStub({"close-the-loop": _mk_skill()})
    refiner = SkillRefiner(
        llm_providers={"cheap": provider},
        store=store,
        auto_refine_on_success=False,
    )

    result = await refiner.refine_from_turn(
        role=_mk_role(),
        agent_handle="alice",
        skills_used=["close-the-loop"],
        review_outcome="done",
        task_summary="",
        plan_summary="",
        turn_id="t",
    )

    assert result is None
    assert provider.calls == []


async def test_llm_exception_swallowed() -> None:
    class _Boom:
        model = "boom"

        async def complete(self, *args, **kwargs):
            raise RuntimeError("provider down")

    store = _StoreStub({"close-the-loop": _mk_skill()})
    refiner = SkillRefiner(llm_providers={"cheap": _Boom()}, store=store)

    result = await refiner.refine_from_turn(
        role=_mk_role(),
        agent_handle="alice",
        skills_used=["close-the-loop"],
        review_outcome="done",
        task_summary="",
        plan_summary="",
        turn_id="t",
    )

    assert result is None
    assert store.updates == []


async def test_store_update_exception_swallowed() -> None:
    class _BoomStore(_StoreStub):
        async def update(self, *a, **k):
            raise RuntimeError("db down")

    provider = _ScriptedProvider(
        [Completion(content=json.dumps({"note": "observation"}))]
    )
    store = _BoomStore({"close-the-loop": _mk_skill()})
    refiner = SkillRefiner(llm_providers={"cheap": provider}, store=store)

    result = await refiner.refine_from_turn(
        role=_mk_role(),
        agent_handle="alice",
        skills_used=["close-the-loop"],
        review_outcome="done",
        task_summary="",
        plan_summary="",
        turn_id="t",
    )

    assert result is None


async def test_empty_skills_used_noops() -> None:
    store = _StoreStub()
    refiner = SkillRefiner(llm_providers={"cheap": _ScriptedProvider([])}, store=store)

    result = await refiner.refine_from_turn(
        role=_mk_role(),
        agent_handle="alice",
        skills_used=[],
        review_outcome="done",
        task_summary="",
        plan_summary="",
        turn_id="t",
    )

    assert result is None
