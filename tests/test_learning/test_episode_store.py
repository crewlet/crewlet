"""Unit tests for ``EpisodeStore``.

Uses a stub ``Database`` so these are pure unit tests — no actual
Postgres/pgvector instance required.  The stub captures executed SQL +
args so tests can assert what was written / queried.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any
from uuid import UUID, uuid4

import pytest

from crewlet.learning.episode_store import EpisodeStore
from crewlet.learning.models import Episode
from crewlet.providers.embeddings.memory import MemoryEmbeddingProvider


class _DBStub:
    """Minimal ``Database``-shaped stub for store-level tests.

    Returns ``fetch_rows`` for any query that reads rows -- ``SELECT``
    and ``DELETE ... RETURNING`` -- so lifecycle CRUD tests can
    assert on returned-row counts.  Bare INSERT / UPDATE return ``[]``.
    """

    def __init__(self, fetch_rows: list[dict[str, Any]] | None = None) -> None:
        self.executed: list[tuple[str, tuple[Any, ...]]] = []
        self._fetch_rows = fetch_rows or []

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        self.executed.append((query, args))
        if "SELECT" in query or "RETURNING" in query.upper():
            return list(self._fetch_rows)
        return []


def _mk_episode(**overrides: Any) -> Episode:
    base = {
        "agent_handle": "alice",
        "agent_role": "Engineer",
        "task_id": "task-1",
        "turn_id": uuid4(),
        "started_at": datetime(2026, 4, 23, 10, 0, tzinfo=UTC),
        "ended_at": datetime(2026, 4, 23, 10, 2, tzinfo=UTC),
        "plan_summary": "Plan: do X",
        "task_summary": "Do X for Y",
        "tool_sequence": ["query_knowledge", "jira_comment"],
        "skills_used": [],
        "review_outcome": "done",
        "duration_ms": 120_000,
    }
    base.update(overrides)
    return Episode(**base)


async def test_write_inserts_row_with_embedding() -> None:
    db = _DBStub()
    embeddings = MemoryEmbeddingProvider(dimensions=8)
    store = EpisodeStore(db, embeddings)

    ep = _mk_episode()
    await store.write(ep)

    assert len(db.executed) == 1
    sql, args = db.executed[0]
    assert "INSERT INTO episodes" in sql
    # Positional args: id, handle, role, task_id, turn_id, started, ended,
    # plan_summary, task_summary, tool_sequence, skills_used, outcome,
    # duration, embedding
    assert args[0] == str(ep.id)
    assert args[1] == "alice"
    assert args[2] == "Engineer"
    # JSONB args are serialised strings
    assert json.loads(args[9]) == ["query_knowledge", "jira_comment"]
    assert json.loads(args[10]) == []
    assert args[11] == "done"
    assert args[12] == 120_000
    # pgvector literal starts with '['
    assert args[13].startswith("[") and args[13].endswith("]")


class _BoomEmbeddings:
    """Embedding provider whose ``embed`` always fails -- simulates a
    transient embeddings-API outage."""

    dimensions = 8

    async def embed(self, texts: list[str]) -> list[list[float]]:
        raise RuntimeError("embeddings api down")


async def test_write_degrades_to_null_embedding_on_embed_failure() -> None:
    """A transient embeddings outage must not lose the episode -- the
    row still lands with embedding=NULL (the column is nullable and the
    HNSW index is partial), mirroring AgentDiary."""
    db = _DBStub()
    store = EpisodeStore(db, _BoomEmbeddings())  # type: ignore[arg-type]

    await store.write(_mk_episode())

    assert len(db.executed) == 1
    sql, args = db.executed[0]
    assert "INSERT INTO episodes" in sql
    assert args[13] is None  # embedding NULL, not a crash


async def test_query_returns_empty_when_query_embed_fails() -> None:
    """If the *query text* can't be embedded, vector recall can't run --
    return [] rather than a broken ORDER BY on a NULL similarity."""
    store = EpisodeStore(_DBStub(), _BoomEmbeddings())  # type: ignore[arg-type]
    results = await store.query(agent_handle="alice", query_text="x")
    assert results == []


async def test_query_scopes_to_agent_and_orders_by_similarity() -> None:
    turn_uuid = uuid4()
    stub_rows = [
        {
            "id": UUID("11111111-1111-1111-1111-111111111111"),
            "agent_handle": "alice",
            "agent_role": "Engineer",
            "task_id": "task-1",
            "turn_id": turn_uuid,
            "started_at": datetime(2026, 4, 23, 9, 0, tzinfo=UTC),
            "ended_at": datetime(2026, 4, 23, 9, 2, tzinfo=UTC),
            "plan_summary": "Plan: do X",
            "task_summary": "Do X for Y",
            "tool_sequence": json.dumps(["jira_comment"]),
            "skills_used": json.dumps([]),
            "review_outcome": "done",
            "duration_ms": 60_000,
            "similarity": 0.92,
        }
    ]
    db = _DBStub(fetch_rows=stub_rows)
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))

    results = await store.query(agent_handle="alice", query_text="how to do X", limit=3)

    assert len(results) == 1
    assert results[0].agent_handle == "alice"
    assert results[0].tool_sequence == ["jira_comment"]
    # Inspect the query to confirm agent scoping + ORDER BY similarity.
    # After the kind-aware refactor, args are positional:
    #   $1 = embedding, $2 = agent_handle, $3 = kinds list, $4 = limit
    # (no outcome filter -> only 4 args).
    sql, args = db.executed[0]
    assert "agent_handle = $2" in sql
    assert "kind = ANY($3)" in sql
    assert "ORDER BY similarity DESC" in sql
    assert args[1] == "alice"
    assert args[2] == ["raw", "compacted"]  # default kinds
    assert args[3] == 3


async def test_query_with_outcome_filter_narrows_to_one_outcome() -> None:
    db = _DBStub(fetch_rows=[])
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))

    await store.query(
        agent_handle="alice",
        query_text="do X",
        limit=5,
        outcome_filter="done",
    )

    # Args after kind-aware refactor:
    #   $1 = embedding, $2 = agent_handle, $3 = kinds, $4 = outcome, $5 = limit
    sql, args = db.executed[0]
    assert "review_outcome = $4" in sql
    assert args[1] == "alice"
    assert args[2] == ["raw", "compacted"]
    assert args[3] == "done"
    assert args[4] == 5


async def test_episode_embeddable_text_concatenates_summaries() -> None:
    ep = _mk_episode(task_summary="ts", plan_summary="ps")
    assert ep.embeddable_text == "ts | ps"

    ep2 = _mk_episode(task_summary="", plan_summary="only plan")
    assert ep2.embeddable_text == "only plan"


@pytest.mark.parametrize("outcome", ["done", "self_iterate", "failed"])
async def test_write_accepts_all_review_outcomes(outcome: str) -> None:
    db = _DBStub()
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    ep = _mk_episode(review_outcome=outcome)
    await store.write(ep)
    assert db.executed[0][1][11] == outcome


# --------------------------------------------------------------------
# Lifecycle: write-side compaction trigger
# --------------------------------------------------------------------


class _QueueStub:
    def __init__(self) -> None:
        self.published: list[tuple[str, Any]] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


async def test_write_does_not_trigger_when_threshold_disabled() -> None:
    """``max_raw_episodes_per_agent=0`` (or no event_queue) -> no
    threshold checking, no compaction events."""
    db = _DBStub(fetch_rows=[{"c": 9999}])
    queue = _QueueStub()
    store = EpisodeStore(
        db,
        MemoryEmbeddingProvider(dimensions=8),
        event_queue=queue,
        max_raw_episodes_per_agent=0,
        write_check_every_n=1,
    )
    await store.write(_mk_episode())
    assert queue.published == []


async def test_write_publishes_compaction_event_when_threshold_crossed() -> None:
    """When count(*) returns > threshold, EpisodeStore publishes a
    ``CompactionRequested`` event so the lifecycle worker can drain
    the agent's pool."""
    from crewlet.events.types import CompactionRequested

    # Count-of-raw query returns 600 > 500 threshold.
    db = _DBStub(fetch_rows=[{"c": 600}])
    queue = _QueueStub()
    store = EpisodeStore(
        db,
        MemoryEmbeddingProvider(dimensions=8),
        event_queue=queue,
        max_raw_episodes_per_agent=500,
        write_check_every_n=1,  # check on every write
    )
    await store.write(_mk_episode())
    # Two SQL calls: insert + count(*); plus the event published.
    assert len(queue.published) == 1
    topic, event = queue.published[0]
    assert topic == "crewlet.events.compaction_requested"
    assert isinstance(event, CompactionRequested)
    assert event.agent_handle == "alice"
    assert event.raw_count == 600
    assert event.threshold == 500


async def test_write_skips_publish_when_count_below_threshold() -> None:
    """count(*) <= threshold -> no event."""
    db = _DBStub(fetch_rows=[{"c": 100}])
    queue = _QueueStub()
    store = EpisodeStore(
        db,
        MemoryEmbeddingProvider(dimensions=8),
        event_queue=queue,
        max_raw_episodes_per_agent=500,
        write_check_every_n=1,
    )
    await store.write(_mk_episode())
    assert queue.published == []


async def test_write_check_every_n_amortises_count_calls() -> None:
    """With write_check_every_n=3, only every 3rd write actually runs
    the count(*) query.  The rest just bump the in-memory counter."""
    db = _DBStub(fetch_rows=[{"c": 50}])
    queue = _QueueStub()
    store = EpisodeStore(
        db,
        MemoryEmbeddingProvider(dimensions=8),
        event_queue=queue,
        max_raw_episodes_per_agent=500,
        write_check_every_n=3,
    )
    for _ in range(5):
        await store.write(_mk_episode())
    # 5 writes -> 5 INSERTs + 1 SELECT count(*) (the one that crossed
    # write_check_every_n=3); the other 2 cycles are still pending.
    selects = [sql for sql, _ in db.executed if "count(*)" in sql]
    assert len(selects) == 1


async def test_compacted_write_does_not_re_trigger() -> None:
    """``insert_compacted`` writes a ``kind='compacted'`` row; that
    must not re-trigger the lifecycle (would loop)."""
    db = _DBStub(fetch_rows=[{"c": 9999}])
    queue = _QueueStub()
    store = EpisodeStore(
        db,
        MemoryEmbeddingProvider(dimensions=8),
        event_queue=queue,
        max_raw_episodes_per_agent=500,
        write_check_every_n=1,
    )
    ep = _mk_episode()
    compacted = ep.model_copy(
        update={
            "kind": "compacted",
            "count": 10,
            "common_task_pattern": "Routine",
            "common_outcome": "done",
        }
    )
    await store.insert_compacted(compacted)
    assert queue.published == []


# --------------------------------------------------------------------
# Lifecycle: targeted deletes + compaction-pool fetch
# --------------------------------------------------------------------


async def test_count_raw_returns_value_from_db() -> None:
    db = _DBStub(fetch_rows=[{"c": 42}])
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    n = await store.count_raw_for_agent("alice")
    assert n == 42
    sql, args = db.executed[0]
    assert "kind = 'raw'" in sql
    assert args[0] == "alice"


async def test_count_raw_handles_missing_value() -> None:
    db = _DBStub(fetch_rows=[])
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    assert await store.count_raw_for_agent("alice") == 0


async def test_mark_consolidated_updates_only_unstamped_rows() -> None:
    """SQL has the ``WHERE consolidated_into_skill_id IS NULL`` guard so
    the call is idempotent on repeat with the same skill_id."""
    db = _DBStub()
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    skill_id = str(uuid4())
    ids = [str(uuid4()), str(uuid4())]
    n = await store.mark_consolidated(episode_ids=ids, skill_id=skill_id)
    assert n == 2  # claim count = ids passed in
    sql, args = db.executed[0]
    assert "UPDATE episodes" in sql
    assert "consolidated_into_skill_id IS NULL" in sql
    assert args[0] == skill_id
    assert args[1] == ids


async def test_mark_consolidated_noop_on_empty_input() -> None:
    db = _DBStub()
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    assert await store.mark_consolidated(episode_ids=[], skill_id="x") == 0
    assert db.executed == []


async def test_delete_non_terminal_uses_age_window() -> None:
    db = _DBStub(fetch_rows=[{}, {}, {}])
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    n = await store.delete_non_terminal_older_than_days(agent_handle="alice", days=14)
    assert n == 3
    sql, args = db.executed[0]
    assert "DELETE FROM episodes" in sql
    assert "review_outcome = 'self_iterate'" in sql
    assert args[0] == "alice"
    assert args[1] == 14


async def test_delete_consolidated_uses_grace_window() -> None:
    db = _DBStub(fetch_rows=[{}])
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    await store.delete_consolidated_older_than_days(agent_handle="alice", days=30)
    sql, args = db.executed[0]
    assert "DELETE FROM episodes" in sql
    assert "consolidated_into_skill_id IS NOT NULL" in sql
    assert args[1] == 30


async def test_delete_compacted_skipped_when_days_zero() -> None:
    """Default 0 = disabled; no SQL fired."""
    db = _DBStub()
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    n = await store.delete_compacted_older_than_days(agent_handle="alice", days=0)
    assert n == 0
    assert db.executed == []


async def test_fetch_raw_for_compaction_excludes_consolidated_and_recent() -> None:
    """The compaction pool query filters by all the right gates."""
    db = _DBStub(fetch_rows=[])
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    await store.fetch_raw_for_compaction(
        agent_handle="alice", older_than_days=30, limit=200
    )
    sql, args = db.executed[0]
    assert "kind = 'raw'" in sql
    assert "consolidated_into_skill_id IS NULL" in sql
    assert "review_outcome IN ('done', 'failed')" in sql
    assert "ORDER BY ended_at ASC" in sql
    assert args[0] == "alice"
    assert args[1] == 30
    assert args[2] == 200


async def test_delete_episodes_by_ids_returns_count() -> None:
    db = _DBStub(fetch_rows=[{}, {}])
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    n = await store.delete_episodes_by_ids([str(uuid4()), str(uuid4())])
    assert n == 2


async def test_delete_episodes_by_ids_noop_on_empty() -> None:
    db = _DBStub()
    store = EpisodeStore(db, MemoryEmbeddingProvider(dimensions=8))
    assert await store.delete_episodes_by_ids([]) == 0
    assert db.executed == []


async def test_compacted_episode_uses_common_pattern_for_embedding() -> None:
    """``embeddable_text`` returns the cluster's common_task_pattern
    for compacted rows so vector similarity surfaces the right
    aggregate when the planner asks about a similar topic."""
    ep = _mk_episode().model_copy(
        update={
            "kind": "compacted",
            "common_task_pattern": "Routine slack replies",
        }
    )
    assert ep.embeddable_text == "Routine slack replies"
