"""Tests for the ``reflect_and_persist`` builtin tool.

The tool writes only at agent scope and uses the
LONG / SHORT classification metadata that PersistDecider produces, so
both write paths flow through the same personal-memory digest at read
time.  Unit-scope and org-scope writes are not reachable through
this builtin -- team-shared rules belong in docs (escalate via
slack_message / escalate), not in personal memory.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

from crewlet.learning.tools import (
    register_reflect_and_persist_tool,
    was_reflect_and_persist_called,
)
from crewlet.tools.protocol import AgentContext
from crewlet.tools.registry import ToolRegistry


class _DiaryStub:
    """Stand-in for :class:`~crewlet.learning.diary.AgentDiary`.

    Records each ``write`` call so tests can assert on the structured
    column values (``agent_id``, ``kind``, ``ttl_until``, ``source``)
    that the tool passes directly to the diary.
    """

    def __init__(self) -> None:
        self.writes: list[dict[str, Any]] = []

    async def write(
        self,
        *,
        agent_id: str,
        kind: str,
        content: str,
        ttl_until: datetime | None = None,
        source: str = "",
        turn_id: str = "",
        metadata: dict[str, Any] | None = None,
    ) -> str:
        self.writes.append(
            {
                "agent_id": agent_id,
                "kind": kind,
                "content": content,
                "ttl_until": ttl_until,
                "source": source,
                "turn_id": turn_id,
                "metadata": metadata or {},
            }
        )
        return "doc-42"


def _ctx(
    *, diary: Any = None, agent_id: str = "alice-id", agent_handle: str = "alice"
) -> AgentContext:
    return AgentContext(
        agent_id=agent_id,
        agent_handle=agent_handle,
        role="Engineer",
        current_task_id="t1",
        agent_diary=diary,
    )


async def test_registration_creates_plan_phase_tool() -> None:
    registry = ToolRegistry()
    diary = _DiaryStub()
    register_reflect_and_persist_tool(registry, diary)  # type: ignore[arg-type]
    tool = registry.get("reflect_and_persist")
    assert tool is not None
    assert tool.parameters["required"] == ["content"]
    # scope is not a parameter -- writes are always
    # agent-scope.  ttl_days is the lever (LONG vs SHORT).
    assert "scope" not in tool.parameters["properties"]
    assert "ttl_days" in tool.parameters["properties"]


async def test_default_writes_at_agent_scope_with_long_metadata() -> None:
    registry = ToolRegistry()
    diary = _DiaryStub()
    register_reflect_and_persist_tool(registry, diary)  # type: ignore[arg-type]
    tool = registry.get("reflect_and_persist")
    assert tool is not None

    result = await tool.execute(
        {"content": "User prefers terse replies."}, _ctx(diary=diary)
    )

    assert result.success
    assert len(diary.writes) == 1
    write = diary.writes[0]
    assert write["agent_id"] == "alice-id"
    assert write["kind"] == "diary_long"
    assert write["source"] == "reflect_and_persist"
    assert write["metadata"]["agent_handle"] == "alice"
    assert write["metadata"]["task_id"] == "t1"
    # No expiry on LONG memories.
    assert write["ttl_until"] is None


async def test_ttl_days_promotes_write_to_short_with_expiry() -> None:
    registry = ToolRegistry()
    diary = _DiaryStub()
    register_reflect_and_persist_tool(registry, diary)  # type: ignore[arg-type]
    tool = registry.get("reflect_and_persist")
    assert tool is not None

    result = await tool.execute(
        {"content": "Sarah is OOO until Friday.", "ttl_days": 7},
        _ctx(diary=diary),
    )
    assert result.success
    write = diary.writes[0]
    assert write["agent_id"] == "alice-id"
    assert write["kind"] == "diary_short"
    assert write["ttl_until"] is not None
    expiry = write["ttl_until"]
    delta = expiry - datetime.now(expiry.tzinfo)
    # Roughly 7 days out, allow slack for clock granularity.
    assert 6 <= delta.days <= 7


async def test_invalid_ttl_days_rejected() -> None:
    registry = ToolRegistry()
    diary = _DiaryStub()
    register_reflect_and_persist_tool(registry, diary)  # type: ignore[arg-type]
    tool = registry.get("reflect_and_persist")
    assert tool is not None

    result = await tool.execute(
        {"content": "x", "ttl_days": "not-a-number"}, _ctx(diary=diary)
    )
    assert not result.success
    assert result.error is not None and "ttl_days" in result.error
    assert diary.writes == []


async def test_excessive_ttl_days_clamped_to_max() -> None:
    registry = ToolRegistry()
    diary = _DiaryStub()
    register_reflect_and_persist_tool(registry, diary)  # type: ignore[arg-type]
    tool = registry.get("reflect_and_persist")
    assert tool is not None

    await tool.execute({"content": "x", "ttl_days": 9999}, _ctx(diary=diary))
    write = diary.writes[0]
    expiry = write["ttl_until"]
    delta = expiry - datetime.now(expiry.tzinfo)
    # Capped at 180 days.
    assert 179 <= delta.days <= 180


async def test_empty_content_rejected() -> None:
    registry = ToolRegistry()
    register_reflect_and_persist_tool(registry, _DiaryStub())  # type: ignore[arg-type]
    tool = registry.get("reflect_and_persist")
    assert tool is not None
    result = await tool.execute({"content": ""}, _ctx(diary=_DiaryStub()))
    assert not result.success
    assert result.error is not None and "content" in result.error


async def test_missing_agent_id_rejected() -> None:
    """Agent-scope writes need agent_id; without it, the digest path
    can't read the row back.  Reject rather than silently orphan."""
    registry = ToolRegistry()
    diary = _DiaryStub()
    register_reflect_and_persist_tool(registry, diary)  # type: ignore[arg-type]
    tool = registry.get("reflect_and_persist")
    assert tool is not None
    result = await tool.execute({"content": "x"}, _ctx(diary=diary, agent_id=""))
    assert not result.success
    assert result.error is not None and "agent_id" in result.error
    assert diary.writes == []


async def test_note_goes_into_metadata_not_content() -> None:
    registry = ToolRegistry()
    diary = _DiaryStub()
    register_reflect_and_persist_tool(registry, diary)  # type: ignore[arg-type]
    tool = registry.get("reflect_and_persist")
    assert tool is not None

    await tool.execute(
        {"content": "fact.", "note": "observed during onboarding"},
        _ctx(diary=diary),
    )

    write = diary.writes[0]
    assert write["content"] == "fact."
    assert write["metadata"]["note"] == "observed during onboarding"


def test_was_reflect_and_persist_called() -> None:
    assert was_reflect_and_persist_called(["search", "reflect_and_persist"]) is True
    assert was_reflect_and_persist_called(["search", "lookup_colleague"]) is False
    assert was_reflect_and_persist_called([]) is False
