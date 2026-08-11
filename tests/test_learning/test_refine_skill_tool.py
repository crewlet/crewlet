"""Tests for the ``refine_skill`` builtin."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from crewlet.learning.models import SynthesizedSkill
from crewlet.learning.tools import register_refine_skill_tool
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry


class _StoreStub:
    def __init__(self, skill: SynthesizedSkill | None) -> None:
        self._skill = skill
        self.updates: list[dict[str, Any]] = []
        self.fetches: list[dict[str, str]] = []

    async def fetch(self, *, agent_handle: str, name: str) -> SynthesizedSkill | None:
        self.fetches.append({"agent_handle": agent_handle, "name": name})
        return self._skill

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
            }
        )
        return skill.model_copy(update={"version": skill.version + 1})

    # Protocol members not used by refine_skill; provided for typing.
    async def insert(self, skill):  # pragma: no cover
        return skill

    async def list_for_agent(self, *a, **k):  # pragma: no cover
        return []

    async def count_for_agent(self, *a, **k):  # pragma: no cover
        return 0

    async def existing_tool_sequences(self, *a, **k):  # pragma: no cover
        return []

    async def list_for_agent_handles(self, *a, **k):  # pragma: no cover
        return []

    async def list_versions(self, *a, **k):  # pragma: no cover
        return []

    async def rollback_to_version(self, **k):  # pragma: no cover
        return None


def _mk_skill() -> SynthesizedSkill:
    return SynthesizedSkill(
        id=uuid4(),
        agent_handle="alice",
        name="close-the-loop",
        description="Close the loop.",
        content="## Close the loop\n1. Ship.",
        tool_sequence=["slack_message"],
        version=1,
        created_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        updated_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    )


def _ctx(*, store: Any) -> AgentContext:
    return AgentContext(
        agent_id="alice-id",
        agent_handle="alice",
        role="Engineer",
        current_task_id="",
        unit_ids=["Eng"],
        synthesized_skill_store=store,
    )


async def test_append_adds_to_body_and_bumps_version() -> None:
    registry = ToolRegistry()
    store = _StoreStub(_mk_skill())
    register_refine_skill_tool(registry, store)
    tool = registry.get("refine_skill")
    assert tool is not None

    result = await tool.execute(
        {
            "name": "close-the-loop",
            "action": "append",
            "content": "3. Close the ticket.",
            "reason": "missed step",
        },
        _ctx(store=store),
    )

    assert result.success
    assert "version=2" in result.output
    assert len(store.updates) == 1
    updated_body = store.updates[0]["skill"].content
    assert "1. Ship." in updated_body
    assert "3. Close the ticket." in updated_body
    assert store.updates[0]["refinement_kind"] == "refine_skill_tool"
    assert store.updates[0]["refinement_note"] == "missed step"


async def test_replace_body_overwrites() -> None:
    registry = ToolRegistry()
    store = _StoreStub(_mk_skill())
    register_refine_skill_tool(registry, store)
    tool = registry.get("refine_skill")
    assert tool is not None

    result = await tool.execute(
        {
            "name": "close-the-loop",
            "action": "replace_body",
            "content": "New procedure only.",
        },
        _ctx(store=store),
    )

    assert result.success
    assert store.updates[0]["skill"].content == "New procedure only."


async def test_unknown_skill_rejected() -> None:
    registry = ToolRegistry()
    store = _StoreStub(None)
    register_refine_skill_tool(registry, store)
    tool = registry.get("refine_skill")
    assert tool is not None

    result = await tool.execute(
        {"name": "nope", "content": "x"},
        _ctx(store=store),
    )

    assert not result.success
    assert result.error is not None and "No synthesized skill" in result.error
    assert store.updates == []


async def test_empty_content_rejected() -> None:
    registry = ToolRegistry()
    store = _StoreStub(_mk_skill())
    register_refine_skill_tool(registry, store)
    tool = registry.get("refine_skill")
    assert tool is not None

    result = await tool.execute(
        {"name": "close-the-loop", "content": ""},
        _ctx(store=store),
    )

    assert not result.success
    assert result.error is not None and "content" in result.error


async def test_invalid_action_rejected() -> None:
    registry = ToolRegistry()
    store = _StoreStub(_mk_skill())
    register_refine_skill_tool(registry, store)
    tool = registry.get("refine_skill")
    assert tool is not None

    result = await tool.execute(
        {"name": "close-the-loop", "action": "bogus", "content": "x"},
        _ctx(store=store),
    )

    assert not result.success
    assert result.error is not None and "action" in result.error


async def test_body_clipped_to_cap() -> None:
    registry = ToolRegistry()
    store = _StoreStub(_mk_skill())
    register_refine_skill_tool(registry, store, max_body_chars=50)
    tool = registry.get("refine_skill")
    assert tool is not None

    result = await tool.execute(
        {
            "name": "close-the-loop",
            "action": "replace_body",
            "content": "x" * 200,
        },
        _ctx(store=store),
    )

    assert result.success
    assert len(store.updates[0]["skill"].content) <= 50


async def test_store_exception_returns_error() -> None:
    class _BoomStore(_StoreStub):
        async def update(self, *a, **k):
            raise RuntimeError("db down")

    registry = ToolRegistry()
    store = _BoomStore(_mk_skill())
    register_refine_skill_tool(registry, store)
    tool = registry.get("refine_skill")
    assert tool is not None

    result = await tool.execute(
        {"name": "close-the-loop", "content": "extra"},
        _ctx(store=store),
    )

    assert not result.success
    assert result.error is not None and "update failed" in result.error
