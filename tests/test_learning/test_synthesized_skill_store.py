"""Unit tests for :class:`SynthesizedSkillStore`.

Stubs the asyncpg layer so these are pure unit tests — no Postgres
required.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from crewlet.learning.models import SynthesizedSkill
from crewlet.learning.synthesized_skill_store import SynthesizedSkillStore


class _DBStub:
    def __init__(
        self,
        *,
        fetch_rows: list[dict[str, Any]] | None = None,
        count_rows: list[dict[str, Any]] | None = None,
        list_rows: list[dict[str, Any]] | None = None,
        sequences_rows: list[dict[str, Any]] | None = None,
    ) -> None:
        self.executed: list[tuple[str, tuple[Any, ...]]] = []
        self._fetch_rows = fetch_rows or []
        self._count_rows = count_rows or []
        self._list_rows = list_rows or []
        self._sequences_rows = sequences_rows or []

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        self.executed.append((query, args))
        q = query.strip()
        if q.startswith("INSERT"):
            return []
        if "SELECT COUNT" in q:
            return list(self._count_rows)
        if "ORDER BY created_at DESC" in q:
            return list(self._list_rows)
        if "SELECT tool_sequence" in q:
            return list(self._sequences_rows)
        return list(self._fetch_rows)


def _mk_skill(**overrides: Any) -> SynthesizedSkill:
    base = {
        "agent_handle": "alice",
        "name": "close-the-loop",
        "description": "Close the feedback loop after a ship.",
        "content": "## Close the loop\n1. Ship.\n2. Post update.",
        "frontmatter": {},
        "tool_sequence": ["slack_message", "jira_comment"],
        "source_episode_ids": [],
        "version": 1,
        "created_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        "updated_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    }
    base.update(overrides)
    return SynthesizedSkill(**base)


async def test_insert_sends_correct_args() -> None:
    db = _DBStub()
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    skill = _mk_skill()
    await store.insert(skill)
    assert len(db.executed) == 1
    sql, args = db.executed[0]
    assert "INSERT INTO synthesized_skills" in sql
    # Column order:
    # id, agent_handle, name, description, content,
    # frontmatter, tool_sequence, source_episode_ids, version
    assert args[0] == str(skill.id)
    assert args[1] == "alice"
    assert args[2] == "close-the-loop"
    assert args[3] == "Close the feedback loop after a ship."
    assert json.loads(args[5]) == {}
    assert json.loads(args[6]) == ["slack_message", "jira_comment"]
    assert json.loads(args[7]) == []
    assert args[8] == 1


async def test_fetch_returns_none_for_empty_key() -> None:
    store = SynthesizedSkillStore(_DBStub())  # type: ignore[arg-type]
    assert await store.fetch(agent_handle="", name="x") is None
    assert await store.fetch(agent_handle="alice", name="") is None


async def test_fetch_parses_row() -> None:
    row = {
        "id": uuid4(),
        "agent_handle": "alice",
        "name": "close-the-loop",
        "description": "desc",
        "content": "body",
        "frontmatter": json.dumps({"tag": "x"}),
        "tool_sequence": json.dumps(["slack_message"]),
        "source_episode_ids": json.dumps([str(uuid4())]),
        "version": 2,
        "created_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        "updated_at": datetime(2026, 4, 23, 9, 5, tzinfo=UTC),
    }
    db = _DBStub(fetch_rows=[row])
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    fetched = await store.fetch(agent_handle="alice", name="close-the-loop")
    assert fetched is not None
    assert fetched.frontmatter == {"tag": "x"}
    assert fetched.tool_sequence == ["slack_message"]
    assert fetched.version == 2
    assert len(fetched.source_episode_ids) == 1


async def test_list_for_agent_orders_by_created_at_desc() -> None:
    row = {
        "id": uuid4(),
        "agent_handle": "alice",
        "name": "s1",
        "description": "d",
        "content": "b",
        "frontmatter": "{}",
        "tool_sequence": "[]",
        "source_episode_ids": "[]",
        "version": 1,
        "created_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        "updated_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    }
    db = _DBStub(list_rows=[row])
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    out = await store.list_for_agent("alice")
    assert len(out) == 1
    sql, args = db.executed[0]
    assert "ORDER BY created_at DESC" in sql
    # Default ``include_archived=False`` adds a state filter
    # passed as a positional parameter alongside the agent handle.
    assert args[0] == "alice"
    assert "archived" in args[1:]


async def test_count_for_agent() -> None:
    db = _DBStub(count_rows=[{"n": 3}])
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    assert await store.count_for_agent("alice") == 3


async def test_count_for_agent_zero_when_no_rows() -> None:
    db = _DBStub(count_rows=[])
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    assert await store.count_for_agent("alice") == 0


async def test_existing_tool_sequences_parses_jsonb_string() -> None:
    db = _DBStub(
        sequences_rows=[
            {"tool_sequence": json.dumps(["slack_message", "jira_comment"])},
            {"tool_sequence": []},
        ]
    )
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    sequences = await store.existing_tool_sequences("alice")
    assert sequences == [["slack_message", "jira_comment"], []]


def test_render_skill_md_produces_frontmatter_and_body() -> None:
    skill = _mk_skill()
    rendered = skill.render_skill_md()
    assert rendered.startswith("---")
    assert 'name: "close-the-loop"' in rendered
    assert 'description: "Close the feedback loop after a ship."' in rendered
    # Body lines preserved.
    assert "1. Ship." in rendered


# --- transition_state ------------------------------------------------------


async def test_transition_state_unguarded_updates_unconditionally() -> None:
    """Without ``guard_last_used_at`` the UPDATE has no last_used_at
    predicate. ``RETURNING id`` row present -> returns True."""
    skill_id = uuid4()
    db = _DBStub(fetch_rows=[{"id": skill_id}])
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    ok = await store.transition_state(skill_id, new_state="active")
    assert ok is True
    sql, args = db.executed[0]
    assert "RETURNING id" in sql
    assert "IS NOT DISTINCT FROM" not in sql
    # state + the two timestamp clears for the 'active' branch.
    assert "stale_at = NULL" in sql and "archived_at = NULL" in sql


async def test_transition_state_guard_adds_optimistic_predicate() -> None:
    """``guard_last_used_at`` adds an ``IS NOT DISTINCT FROM``
    predicate so a concurrent ``mark_used`` bump fails the UPDATE."""
    skill_id = uuid4()
    snapshot = datetime(2026, 4, 1, tzinfo=UTC)
    db = _DBStub(fetch_rows=[{"id": skill_id}])
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    ok = await store.transition_state(
        skill_id, new_state="stale", guard_last_used_at=snapshot
    )
    assert ok is True
    sql, args = db.executed[0]
    assert "last_used_at IS NOT DISTINCT FROM $3" in sql
    assert snapshot in args


async def test_transition_state_guard_accepts_none_snapshot() -> None:
    """A never-used skill snapshots ``last_used_at = None``; the guard
    must still be applied (NULL IS NOT DISTINCT FROM NULL) -- ``None``
    is a real guard value, distinct from "no guard"."""
    skill_id = uuid4()
    db = _DBStub(fetch_rows=[{"id": skill_id}])
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    ok = await store.transition_state(
        skill_id, new_state="archived", guard_last_used_at=None
    )
    assert ok is True
    sql, args = db.executed[0]
    assert "last_used_at IS NOT DISTINCT FROM $3" in sql
    assert None in args


async def test_transition_state_returns_false_when_guard_loses_race() -> None:
    """When the guarded UPDATE matches zero rows (a concurrent
    ``mark_used`` bumped ``last_used_at`` past the snapshot), the
    ``RETURNING id`` result is empty and the method returns False."""
    db = _DBStub(fetch_rows=[])  # UPDATE ... RETURNING matched nothing
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    ok = await store.transition_state(
        uuid4(),
        new_state="archived",
        guard_last_used_at=datetime(2026, 4, 1, tzinfo=UTC),
    )
    assert ok is False


async def test_transition_state_rejects_invalid_state() -> None:
    import pytest

    store = SynthesizedSkillStore(_DBStub())  # type: ignore[arg-type]
    with pytest.raises(ValueError, match="invalid state"):
        await store.transition_state(uuid4(), new_state="bogus")
