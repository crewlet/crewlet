"""Tests for phase-5 versioning + rollback on ``SynthesizedSkillStore``."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any
from uuid import UUID, uuid4

from crewlet.learning.models import SynthesizedSkill
from crewlet.learning.synthesized_skill_store import SynthesizedSkillStore


class _DBStub:
    """DB stub that emulates the subset of SQL the store uses.

    Keeps an in-memory ``synthesized_skills`` row keyed by id + a list
    of archived versions.  Pattern-matches on the query string.
    """

    def __init__(self, initial: dict[str, Any] | None = None) -> None:
        self._main: dict[str, dict[str, Any]] = {}
        self._versions: list[dict[str, Any]] = []
        self.executed: list[str] = []
        # When set, the next ``_fetch_by_id`` SELECT returns the row but
        # then bumps its version -- simulating a concurrent writer that
        # landed between this caller's read and its guarded UPDATE.
        self.bump_version_after_next_fetch = False
        if initial is not None:
            self._main[initial["id"]] = dict(initial)

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        self.executed.append(query.strip())
        q = query.strip()

        if q.startswith("INSERT INTO synthesized_skills ("):
            (
                skill_id,
                agent_handle,
                name,
                description,
                content,
                frontmatter,
                tool_sequence,
                source_episode_ids,
                version,
            ) = args
            self._main[skill_id] = {
                "id": UUID(skill_id),
                "agent_handle": agent_handle,
                "name": name,
                "description": description,
                "content": content,
                "frontmatter": frontmatter,
                "tool_sequence": tool_sequence,
                "source_episode_ids": source_episode_ids,
                "version": version,
                "created_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
                "updated_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
            }
            return []

        if q.startswith("SELECT id, agent_handle") and "WHERE id = $1::uuid" in q:
            row = self._main.get(args[0])
            if row is None:
                return []
            snapshot = dict(row)
            if self.bump_version_after_next_fetch:
                # A concurrent writer lands right after this read.
                self.bump_version_after_next_fetch = False
                row["version"] += 1
            return [snapshot]

        if q.startswith("INSERT INTO synthesized_skill_versions ("):
            (
                skill_id,
                agent_handle,
                name,
                description,
                content,
                frontmatter,
                tool_sequence,
                source_episode_ids,
                version,
                refinement_kind,
                refinement_note,
            ) = args
            self._versions.append(
                {
                    "id": uuid4(),
                    "skill_id": UUID(skill_id),
                    "agent_handle": agent_handle,
                    "name": name,
                    "description": description,
                    "content": content,
                    "frontmatter": frontmatter,
                    "tool_sequence": tool_sequence,
                    "source_episode_ids": source_episode_ids,
                    "version": version,
                    "refinement_kind": refinement_kind,
                    "refinement_note": refinement_note,
                    "archived_at": datetime.now(UTC),
                }
            )
            return []

        if q.startswith("UPDATE synthesized_skills") and "SET description" in q:
            # Guarded UPDATE: ... WHERE id = $1 AND version = $8.
            (
                skill_id,
                description,
                content,
                frontmatter,
                tool_sequence,
                source_episode_ids,
                new_version,
                expected_version,
            ) = args
            row = self._main[skill_id]
            # Optimistic-concurrency guard: only the writer whose
            # observed version still matches wins; a stale writer gets
            # zero rows back.
            if row["version"] != expected_version:
                return []
            row["description"] = description
            row["content"] = content
            row["frontmatter"] = frontmatter
            row["tool_sequence"] = tool_sequence
            row["source_episode_ids"] = source_episode_ids
            row["version"] = new_version
            row["updated_at"] = datetime.now(UTC)
            return [{"id": skill_id}]  # RETURNING id

        if q.startswith("DELETE FROM synthesized_skill_versions"):
            skill_id, keep = args
            keep_i = int(keep)
            skill_uuid = UUID(skill_id)
            matching = [v for v in self._versions if v["skill_id"] == skill_uuid]
            # Mirror the store's ``ORDER BY archived_at DESC, version DESC``
            # so the prune tiebreaker is exercised deterministically.
            matching.sort(key=lambda v: (v["archived_at"], v["version"]), reverse=True)
            to_drop = matching[keep_i:]
            self._versions = [v for v in self._versions if v not in to_drop]
            return []

        if q.startswith("SELECT id, skill_id") and "WHERE skill_id = $1::uuid" in q:
            skill_uuid = UUID(args[0])
            matches = [v for v in self._versions if v["skill_id"] == skill_uuid]
            matches.sort(key=lambda v: v["archived_at"], reverse=True)
            return [dict(v) for v in matches[: int(args[1])]]

        if q.startswith("SELECT id, skill_id") and "WHERE id = $1::uuid" in q:
            target_id = UUID(args[0])
            for v in self._versions:
                if v["id"] == target_id:
                    return [dict(v)]
            return []

        return []


def _mk_initial_skill() -> SynthesizedSkill:
    return SynthesizedSkill(
        id=uuid4(),
        agent_handle="alice",
        name="close-the-loop",
        description="desc v1",
        content="body v1",
        frontmatter={},
        tool_sequence=["slack_message"],
        source_episode_ids=[],
        version=1,
        created_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
        updated_at=datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
    )


async def _seed_store(skill: SynthesizedSkill) -> tuple[SynthesizedSkillStore, _DBStub]:
    db = _DBStub()
    store = SynthesizedSkillStore(db)  # type: ignore[arg-type]
    await store.insert(skill)
    return store, db


async def test_update_archives_prior_state_and_bumps_version() -> None:
    initial = _mk_initial_skill()
    store, db = await _seed_store(initial)
    patched = initial.model_copy(update={"content": "body v2"})

    result = await store.update(
        patched,
        refinement_kind="refine_skill_tool",
        refinement_note="appended step",
    )

    assert result.version == 2
    assert result.content == "body v2"
    # Exactly one version archived — the prior state.
    versions = await store.list_versions(initial.id)
    assert len(versions) == 1
    assert versions[0].version == 1
    assert versions[0].content == "body v1"
    assert versions[0].refinement_kind == "refine_skill_tool"
    assert versions[0].refinement_note == "appended step"


async def test_update_retries_on_concurrent_version_bump() -> None:
    """A concurrent refine that bumps the version between this caller's
    read and its guarded UPDATE makes the first attempt's guard miss;
    ``update`` re-reads and retries.  The version sequence stays
    gap-free and exactly one prior state is archived per winning write.
    """
    initial = _mk_initial_skill()  # version 1
    store, db = await _seed_store(initial)
    db.bump_version_after_next_fetch = True  # simulate a racing writer
    patched = initial.model_copy(update={"content": "body v2"})

    result = await store.update(patched, refinement_kind="refine_skill_tool")

    # Concurrent bump 1 -> 2, then our guarded write 2 -> 3.  No gap.
    assert result.version == 3
    assert result.content == "body v2"
    # One UPDATE missed the guard and retried, so two UPDATE statements
    # were issued.
    update_calls = [q for q in db.executed if q.startswith("UPDATE synthesized_skills")]
    assert len(update_calls) == 2
    # Exactly one version row archived -- the state our winning write
    # replaced (version 2), not the stale one we first read.
    versions = await store.list_versions(initial.id)
    assert len(versions) == 1
    assert versions[0].version == 2


async def test_list_versions_orders_newest_first() -> None:
    initial = _mk_initial_skill()
    store, _ = await _seed_store(initial)

    patched_a = initial.model_copy(update={"content": "body v2"})
    await store.update(patched_a, refinement_kind="observed_in_practice")
    patched_b = patched_a.model_copy(update={"content": "body v3", "version": 2})
    await store.update(patched_b, refinement_kind="counter_example")

    versions = await store.list_versions(initial.id)
    assert [v.version for v in versions] == [2, 1]
    assert versions[0].content == "body v2"


async def test_rollback_to_version_writes_forward_row() -> None:
    initial = _mk_initial_skill()
    store, _ = await _seed_store(initial)

    patched = initial.model_copy(update={"content": "body v2"})
    await store.update(patched, refinement_kind="refine_skill_tool")
    patched2 = patched.model_copy(update={"content": "body v3", "version": 2})
    await store.update(patched2, refinement_kind="refine_skill_tool")

    versions = await store.list_versions(initial.id)
    # versions[1] is the v1 row — rolling back to it should restore "body v1".
    rollback = await store.rollback_to_version(version_id=versions[1].id)

    assert rollback is not None
    assert rollback.content == "body v1"
    # Version increments forward; we don't rewind the counter.
    assert rollback.version == 4
    # An additional history row captured the state just before the rollback.
    later_versions = await store.list_versions(initial.id)
    kinds = [v.refinement_kind for v in later_versions]
    assert kinds[0] == "rollback"


async def test_prune_keeps_only_most_recent_n_versions() -> None:
    initial = _mk_initial_skill()
    store, _ = await _seed_store(initial)

    content = "body v1"
    skill = initial
    for i in range(6):
        content = f"body v{i + 2}"
        skill = skill.model_copy(update={"content": content, "version": skill.version})
        skill = await store.update(
            skill,
            refinement_kind="refine_skill_tool",
            max_versions_kept=3,
        )

    versions = await store.list_versions(initial.id)
    assert len(versions) == 3


async def test_rollback_unknown_version_returns_none() -> None:
    initial = _mk_initial_skill()
    store, _ = await _seed_store(initial)

    assert await store.rollback_to_version(version_id=uuid4()) is None


async def test_update_unknown_skill_raises() -> None:
    store = SynthesizedSkillStore(_DBStub())  # type: ignore[arg-type]
    ghost = _mk_initial_skill()
    try:
        await store.update(ghost, refinement_kind="refine_skill_tool")
    except ValueError as exc:
        assert "unknown synthesized skill" in str(exc)
    else:
        raise AssertionError("expected ValueError")


async def test_update_preserves_frontmatter_and_tool_sequence() -> None:
    initial = _mk_initial_skill()
    store, db = await _seed_store(initial)

    patched = initial.model_copy(
        update={
            "content": "body v2",
            "frontmatter": {"category": "ops"},
            "tool_sequence": ["slack_message", "jira_comment"],
        }
    )
    result = await store.update(patched, refinement_kind="refine_skill_tool")

    assert result.frontmatter == {"category": "ops"}
    assert result.tool_sequence == ["slack_message", "jira_comment"]
    # Archived v1 still has the original single-tool sequence.
    versions = await store.list_versions(initial.id)
    assert versions[0].tool_sequence == ["slack_message"]
