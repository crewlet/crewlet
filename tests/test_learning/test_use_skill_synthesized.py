"""Tests for the ``use_skill`` builtin (synthesized-only).

``use_skill`` resolves only the agent's own synthesized skills.
Team-published knowledge-base pages are reached via the role's
knowledge-base search / get-page MCP tools.
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from crewlet.learning.models import SynthesizedSkill
from crewlet.tools.builtin import register_builtin_tools
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry


class _SynthesizedStoreStub:
    def __init__(self, skill: SynthesizedSkill | None) -> None:
        self._skill = skill
        self.fetches: list[dict[str, str]] = []
        self.marked_used_ids: list[Any] = []

    async def fetch(self, *, agent_handle: str, name: str) -> SynthesizedSkill | None:
        self.fetches.append({"agent_handle": agent_handle, "name": name})
        return self._skill

    async def mark_used(self, skill_id):
        self.marked_used_ids.append(skill_id)
        return None

    async def insert(  # pragma: no cover
        self, skill: SynthesizedSkill
    ) -> SynthesizedSkill:
        return skill

    async def list_for_agent(self, agent_handle: str):  # pragma: no cover
        return []

    async def count_for_agent(self, agent_handle: str) -> int:  # pragma: no cover
        return 0

    async def existing_tool_sequences(self, agent_handle: str):  # pragma: no cover
        return []


def _mk_synthesized_skill() -> SynthesizedSkill:
    return SynthesizedSkill(
        id=uuid4(),
        agent_handle="alice",
        name="close-the-loop",
        description="Close the loop after ship.",
        content="## Close the loop\n1. Ship.\n2. Post update.",
        created_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        updated_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    )


def _ctx(
    *,
    synthesized_store: Any = None,
    agent_handle: str = "alice",
) -> AgentContext:
    return AgentContext(
        agent_id="alice-id",
        agent_handle=agent_handle,
        role="Engineer",
        current_task_id="",
        synthesized_skill_store=synthesized_store,
    )


async def test_use_skill_loads_synthesized_when_present() -> None:
    registry = ToolRegistry()
    register_builtin_tools(registry)
    tool = registry.get("use_skill")
    assert tool is not None

    store = _SynthesizedStoreStub(_mk_synthesized_skill())
    result = await tool.execute(
        {"skill_name": "close-the-loop"},
        _ctx(synthesized_store=store),
    )

    assert result.success
    assert "close-the-loop" in result.output
    assert "Close the loop after ship." in result.output
    assert "1. Ship." in result.output
    assert store.fetches[0]["agent_handle"] == "alice"


async def test_use_skill_returns_error_when_synthesized_miss() -> None:
    """A miss points the agent at its knowledge-base search tool
    for team-published procedural docs -- those are not the
    responsibility of ``use_skill``.  The pointer is phrased by
    capability, not by a hardcoded tool name."""
    registry = ToolRegistry()
    register_builtin_tools(registry)
    tool = registry.get("use_skill")
    assert tool is not None

    store = _SynthesizedStoreStub(None)
    result = await tool.execute(
        {"skill_name": "code-review"},
        _ctx(synthesized_store=store),
    )
    assert not result.success
    assert result.error is not None
    assert "No synthesized skill" in result.error
    assert "knowledge-base search tool" in result.error
    assert "confluence_search" not in result.error


async def test_use_skill_without_store_returns_error() -> None:
    registry = ToolRegistry()
    register_builtin_tools(registry)
    tool = registry.get("use_skill")
    assert tool is not None

    result = await tool.execute(
        {"skill_name": "whatever"},
        _ctx(synthesized_store=None),
    )
    assert not result.success
    assert result.error is not None
    assert "store unavailable" in result.error
    assert "knowledge-base search tool" in result.error


async def test_use_skill_store_exception_returns_error() -> None:
    class _BoomStore:
        async def fetch(self, **kwargs):
            raise RuntimeError("db down")

    registry = ToolRegistry()
    register_builtin_tools(registry)
    tool = registry.get("use_skill")
    assert tool is not None

    result = await tool.execute(
        {"skill_name": "x"},
        _ctx(synthesized_store=_BoomStore()),
    )
    assert not result.success
    assert result.error is not None and "skill lookup failed" in result.error


async def test_use_skill_missing_skill_name_param() -> None:
    registry = ToolRegistry()
    register_builtin_tools(registry)
    tool = registry.get("use_skill")
    assert tool is not None

    result = await tool.execute({"skill_name": ""}, _ctx())
    assert not result.success
    assert result.error is not None
    assert "skill_name is required" in result.error
