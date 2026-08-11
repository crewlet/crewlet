"""Unit tests for :class:`AgentDiary`.

Stubs the asyncpg layer so these are pure unit tests -- no Postgres
required.  Verifies the diary's write surface (structured columns
instead of a metadata blob), the TTL filter on reads, the retrieval-
count bookkeeping (telemetry), and the best-effort failure
handling that keeps personal memory from breaking turns.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime, timedelta
from typing import Any
from uuid import uuid4

from crewlet.learning.diary import KIND_LONG, KIND_SHORT, AgentDiary


class _DBStub:
    def __init__(self) -> None:
        self.executed: list[tuple[str, tuple[Any, ...]]] = []
        self.results: list[list[dict[str, Any]]] = []

    def queue(self, rows: list[dict[str, Any]]) -> None:
        self.results.append(rows)

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        self.executed.append((query, args))
        if self.results:
            return self.results.pop(0)
        return []


# ---------------------------------------------------------------------------
# write
# ---------------------------------------------------------------------------


def _insert_call(db: _DBStub) -> tuple[str, tuple[Any, ...]]:
    """Return the INSERT execute call.

    ``write`` now runs a dedup SELECT before the INSERT, so the INSERT
    is the last execute -- callers want that one, not ``executed[0]``.
    """
    for call in reversed(db.executed):
        if "INSERT INTO agent_diary" in call[0]:
            return call
    raise AssertionError("no INSERT INTO agent_diary execute call found")


async def test_write_diary_long_persists_columns_separately() -> None:
    db = _DBStub()
    diary = AgentDiary(db)  # type: ignore[arg-type]
    out = await diary.write(
        agent_id="alice-id",
        kind=KIND_LONG,
        content="Stakeholder X prefers digests.",
        source="persist_decider",
        turn_id="t1",
        metadata={"agent_handle": "alice"},
    )
    assert out  # uuid string
    # write() runs a dedup SELECT then the INSERT.
    assert len(db.executed) == 2
    sql, args = _insert_call(db)
    assert "INSERT INTO agent_diary" in sql
    # args order: id, agent_id, kind, content, ttl_until, source,
    # turn_id, metadata
    assert args[1] == "alice-id"
    assert args[2] == "diary_long"
    assert args[3] == "Stakeholder X prefers digests."
    assert args[4] is None
    assert args[5] == "persist_decider"
    assert args[6] == "t1"
    assert json.loads(args[7]) == {"agent_handle": "alice"}


async def test_write_diary_short_passes_ttl_until_through() -> None:
    db = _DBStub()
    diary = AgentDiary(db)  # type: ignore[arg-type]
    expiry = datetime(2026, 6, 1, 12, 0, tzinfo=UTC)
    await diary.write(
        agent_id="alice-id",
        kind=KIND_SHORT,
        content="Sarah is OOO until Friday.",
        ttl_until=expiry,
        source="reflect_and_persist",
        turn_id="t2",
    )
    sql, args = _insert_call(db)
    assert args[2] == "diary_short"
    assert args[4] == expiry  # raw datetime, not isoformat string


async def test_write_stores_full_content_without_truncation() -> None:
    db = _DBStub()
    diary = AgentDiary(db)  # type: ignore[arg-type]
    await diary.write(agent_id="alice-id", kind=KIND_LONG, content="x" * 50000)
    _, args = _insert_call(db)
    assert len(args[3]) == 50000  # stored verbatim, never truncated


async def test_write_caps_only_embedding_input_not_stored_content() -> None:
    from crewlet.learning.diary import _MAX_EMBED_INPUT_CHARS

    db = _DBStub()
    embeddings = _EmbeddingStub()
    diary = AgentDiary(db, embeddings)  # type: ignore[arg-type]
    big = "y" * (_MAX_EMBED_INPUT_CHARS + 1234)
    await diary.write(agent_id="alice-id", kind=KIND_LONG, content=big)
    # Full content persisted...
    _, args = _insert_call(db)
    assert len(args[3]) == len(big)
    # ...but the text sent to the embeddings provider is sliced.
    assert len(embeddings.calls[0][0]) == _MAX_EMBED_INPUT_CHARS


async def test_write_dedupes_against_live_row() -> None:
    """An identical live row short-circuits the write: the existing id
    comes back and no second INSERT fires."""
    db = _DBStub()
    existing_id = uuid4()
    db.queue([{"id": existing_id}])  # _find_duplicate hit
    diary = AgentDiary(db)  # type: ignore[arg-type]
    out = await diary.write(
        agent_id="alice-id", kind=KIND_LONG, content="Stakeholder X prefers digests."
    )
    assert out == str(existing_id)
    assert all("INSERT INTO agent_diary" not in sql for sql, _ in db.executed)


async def test_write_rejects_empty_inputs() -> None:
    diary = AgentDiary(_DBStub())  # type: ignore[arg-type]
    for bad in [
        {"agent_id": "", "kind": KIND_LONG, "content": "x"},
        {"agent_id": "a", "kind": "memory_long", "content": "x"},  # unrecognized kind
        {"agent_id": "a", "kind": KIND_LONG, "content": ""},
    ]:
        try:
            await diary.write(**bad)  # type: ignore[arg-type]
        except ValueError:
            continue
        else:  # pragma: no cover - sanity
            raise AssertionError(f"expected ValueError for {bad}")


# ---------------------------------------------------------------------------
# list_for_agent: ordering, TTL filter, kind flattening
# ---------------------------------------------------------------------------


def _row(
    *,
    content: str,
    kind: str = KIND_LONG,
    ttl_until: datetime | None = None,
    source: str = "",
    turn_id: str = "",
    metadata: dict[str, Any] | None = None,
    agent_id: str = "alice-id",
) -> dict[str, Any]:
    return {
        "id": uuid4(),
        "agent_id": agent_id,
        "kind": kind,
        "content": content,
        "ttl_until": ttl_until,
        "source": source,
        "turn_id": turn_id,
        "metadata": json.dumps(metadata or {}),
        "retrieval_count": 0,
        "last_retrieved_at": None,
        "created_at": datetime(2026, 5, 3, 12, 0, tzinfo=UTC),
    }


async def test_list_for_agent_flattens_kind_into_metadata() -> None:
    """Read-side bridge: the diary returns dicts whose
    ``metadata.kind`` mirrors the stored kind so consumers
    (personal_memory, persist_decider dedup) read it unchanged."""
    db = _DBStub()
    db.queue([_row(content="Memory body", kind=KIND_LONG)])
    diary = AgentDiary(db)  # type: ignore[arg-type]
    out = await diary.list_for_agent("alice-id")
    assert len(out) == 1
    doc = out[0]
    assert doc["content"] == "Memory body"
    assert doc["metadata"]["kind"] == KIND_LONG
    # source / turn_id flatten into metadata when set; not present
    # here so they stay absent.
    assert "source" not in doc["metadata"]


async def test_list_for_agent_flattens_ttl_until_when_set() -> None:
    db = _DBStub()
    # Use a future TTL so the row isn't filtered as expired — expiry
    # filtering is covered by test_list_for_agent_filters_expired_rows;
    # this test only asserts ttl_until flattens into metadata.
    expiry = datetime.now(UTC) + timedelta(days=10)
    db.queue([_row(content="Sarah OOO", kind=KIND_SHORT, ttl_until=expiry)])
    diary = AgentDiary(db)  # type: ignore[arg-type]
    out = await diary.list_for_agent("alice-id")
    assert out[0]["metadata"]["ttl_until"] == expiry.isoformat()


async def test_list_for_agent_filters_expired_rows() -> None:
    """TTL filtering happens at the diary read layer.  Consumers see
    only fresh rows by default."""
    db = _DBStub()
    fresh = datetime.now(UTC) + timedelta(days=10)
    expired = datetime.now(UTC) - timedelta(days=1)
    db.queue(
        [
            _row(content="Fresh", kind=KIND_SHORT, ttl_until=fresh),
            _row(content="Stale", kind=KIND_SHORT, ttl_until=expired),
        ]
    )
    diary = AgentDiary(db)  # type: ignore[arg-type]
    out = await diary.list_for_agent("alice-id")
    contents = [d["content"] for d in out]
    assert "Fresh" in contents
    assert "Stale" not in contents


async def test_list_for_agent_include_expired_returns_everything() -> None:
    db = _DBStub()
    expired = datetime.now(UTC) - timedelta(days=1)
    db.queue([_row(content="Stale", kind=KIND_SHORT, ttl_until=expired)])
    diary = AgentDiary(db)  # type: ignore[arg-type]
    out = await diary.list_for_agent("alice-id", include_expired=True)
    assert len(out) == 1


async def test_list_for_agent_swallows_db_failure() -> None:
    """Personal memory must never break a turn -- a DB exception
    inside ``list_for_agent`` returns ``[]`` instead of propagating."""

    class _BoomDB:
        async def execute(self, *args: Any, **kwargs: Any) -> Any:
            raise RuntimeError("connection reset")

    diary = AgentDiary(_BoomDB())  # type: ignore[arg-type]
    out = await diary.list_for_agent("alice-id")
    assert out == []


async def test_list_for_agent_returns_empty_for_blank_id() -> None:
    diary = AgentDiary(_DBStub())  # type: ignore[arg-type]
    assert await diary.list_for_agent("") == []


# ---------------------------------------------------------------------------
# fetch_by_id + count_for_agent
# ---------------------------------------------------------------------------


async def test_fetch_by_id_parses_row() -> None:
    db = _DBStub()
    row = _row(content="Some memory")
    db.queue([row])
    diary = AgentDiary(db)  # type: ignore[arg-type]
    doc = await diary.fetch_by_id(row["id"])
    assert doc is not None
    assert doc["content"] == "Some memory"


async def test_fetch_by_id_returns_none_when_missing() -> None:
    db = _DBStub()
    db.queue([])
    diary = AgentDiary(db)  # type: ignore[arg-type]
    assert await diary.fetch_by_id(uuid4()) is None


async def test_count_for_agent_excludes_expired_by_default() -> None:
    db = _DBStub()
    db.queue([{"n": 4}])
    diary = AgentDiary(db)  # type: ignore[arg-type]
    out = await diary.count_for_agent("alice-id")
    assert out == 4
    sql, _ = db.executed[0]
    assert "ttl_until IS NULL OR ttl_until > now()" in sql


async def test_count_for_agent_include_expired_uses_simpler_query() -> None:
    db = _DBStub()
    db.queue([{"n": 7}])
    diary = AgentDiary(db)  # type: ignore[arg-type]
    out = await diary.count_for_agent("alice-id", include_expired=True)
    assert out == 7
    sql, _ = db.executed[0]
    assert "ttl_until" not in sql


# ---------------------------------------------------------------------------
# mark_retrieved (telemetry)
# ---------------------------------------------------------------------------


async def test_mark_retrieved_issues_one_update() -> None:
    db = _DBStub()
    diary = AgentDiary(db)  # type: ignore[arg-type]
    doc_id = uuid4()
    await diary.mark_retrieved(doc_id)
    assert len(db.executed) == 1
    sql, args = db.executed[0]
    assert "UPDATE agent_diary" in sql
    assert "retrieval_count = retrieval_count + 1" in sql
    assert "last_retrieved_at" in sql
    assert args == (str(doc_id),)


async def test_mark_retrieved_swallows_db_failure() -> None:
    """Telemetry failure must never propagate up to the read path."""

    class _BoomDB:
        async def execute(self, *args: Any, **kwargs: Any) -> Any:
            raise RuntimeError("db down")

    diary = AgentDiary(_BoomDB())  # type: ignore[arg-type]
    # Should not raise.
    await diary.mark_retrieved(uuid4())


# ---------------------------------------------------------------------------
# delete + delete_expired
# ---------------------------------------------------------------------------


async def test_delete_returns_true_on_hit() -> None:
    db = _DBStub()
    db.queue([{"id": uuid4()}])
    diary = AgentDiary(db)  # type: ignore[arg-type]
    assert await diary.delete(uuid4()) is True


async def test_delete_returns_false_on_miss() -> None:
    db = _DBStub()
    db.queue([])
    diary = AgentDiary(db)  # type: ignore[arg-type]
    assert await diary.delete(uuid4()) is False


async def test_delete_expired_returns_count_of_pruned_rows() -> None:
    db = _DBStub()
    db.queue([{"id": uuid4()}, {"id": uuid4()}, {"id": uuid4()}])
    diary = AgentDiary(db)  # type: ignore[arg-type]
    deleted = await diary.delete_expired()
    assert deleted == 3
    sql, _ = db.executed[0]
    assert "ttl_until < now()" in sql


async def test_delete_expired_swallows_failure() -> None:
    class _BoomDB:
        async def execute(self, *args: Any, **kwargs: Any) -> Any:
            raise RuntimeError("rate limited")

    diary = AgentDiary(_BoomDB())  # type: ignore[arg-type]
    assert await diary.delete_expired() == 0


# ---------------------------------------------------------------------------
# write embeds content; search_for_agent does vector search
# ---------------------------------------------------------------------------


class _EmbeddingStub:
    """Returns a deterministic 4-dim vector for any text."""

    def __init__(self, *, raises: bool = False) -> None:
        self.calls: list[list[str]] = []
        self._raises = raises
        self.dim = 4

    async def embed(self, texts: list[str]) -> list[list[float]]:
        if self._raises:
            raise RuntimeError("embeddings api down")
        self.calls.append(list(texts))
        return [[0.1 * (i + 1) for i in range(self.dim)] for _ in texts]

    @property
    def dimensions(self) -> int:
        return self.dim


async def test_write_embeds_content_when_provider_wired() -> None:
    db = _DBStub()
    embeddings = _EmbeddingStub()
    diary = AgentDiary(db, embeddings)  # type: ignore[arg-type]
    await diary.write(
        agent_id="alice-id",
        kind=KIND_LONG,
        content="Stakeholder X prefers digests.",
    )
    assert embeddings.calls == [["Stakeholder X prefers digests."]]
    sql, args = _insert_call(db)
    assert "embedding" in sql
    # args order: id, agent_id, kind, content, ttl_until, source,
    # turn_id, metadata, embedding
    assert args[8] is not None
    assert args[8].startswith("[") and args[8].endswith("]")


async def test_write_handles_embed_failure_with_null_embedding() -> None:
    """A transient embeddings-API outage must not lose the diary
    entry -- the row gets persisted with embedding=NULL so the recency
    half of the prefetch's hybrid candidate selection still surfaces
    it."""
    db = _DBStub()
    embeddings = _EmbeddingStub(raises=True)
    diary = AgentDiary(db, embeddings)  # type: ignore[arg-type]
    await diary.write(
        agent_id="alice-id",
        kind=KIND_LONG,
        content="This still gets stored.",
    )
    sql, args = _insert_call(db)
    assert "INSERT INTO agent_diary" in sql
    assert args[8] is None  # embedding NULL


async def test_write_without_embeddings_provider_persists_with_null() -> None:
    db = _DBStub()
    diary = AgentDiary(db)  # type: ignore[arg-type]  # no embeddings
    await diary.write(
        agent_id="alice-id",
        kind=KIND_LONG,
        content="Content without embedding.",
    )
    sql, args = _insert_call(db)
    assert args[8] is None


async def test_search_for_agent_returns_empty_when_no_embeddings() -> None:
    diary = AgentDiary(_DBStub())  # type: ignore[arg-type]
    out = await diary.search_for_agent("alice-id", query="test", limit=5)
    assert out == []


async def test_search_for_agent_returns_empty_for_blank_inputs() -> None:
    diary = AgentDiary(_DBStub(), _EmbeddingStub())  # type: ignore[arg-type]
    assert await diary.search_for_agent("", query="x") == []
    assert await diary.search_for_agent("alice-id", query="") == []


async def test_search_for_agent_runs_vector_similarity() -> None:
    db = _DBStub()
    embeddings = _EmbeddingStub()
    row = _row(content="Stakeholder X prefers digests")
    row["similarity"] = 0.91
    db.queue([row])
    diary = AgentDiary(db, embeddings)  # type: ignore[arg-type]
    out = await diary.search_for_agent("alice-id", query="how does X want updates")
    assert len(out) == 1
    assert out[0]["similarity"] == 0.91
    assert out[0]["content"] == "Stakeholder X prefers digests"
    sql, args = db.executed[0]
    assert "embedding <=> $1::vector" in sql
    assert "embedding IS NOT NULL" in sql
    # TTL filter on by default.
    assert "ttl_until IS NULL OR ttl_until > now()" in sql
    assert args[1] == "alice-id"


async def test_search_for_agent_min_similarity_drops_low_rows() -> None:
    """Relevance floor on the diary's vector search -- a query the
    agent has no relevant memory for must not surface its
    least-irrelevant diary entries as if they matched."""
    db = _DBStub()
    embeddings = _EmbeddingStub()
    hi = _row(content="Stakeholder X prefers digests")
    hi["similarity"] = 0.71
    lo = _row(content="Unrelated note")
    lo["similarity"] = 0.09
    db.queue([hi, lo])
    diary = AgentDiary(db, embeddings)  # type: ignore[arg-type]
    out = await diary.search_for_agent(
        "alice-id", query="how does X want updates", min_similarity=0.25
    )
    assert len(out) == 1
    assert out[0]["content"] == "Stakeholder X prefers digests"


async def test_search_for_agent_include_expired_drops_ttl_filter() -> None:
    db = _DBStub()
    embeddings = _EmbeddingStub()
    db.queue([])
    diary = AgentDiary(db, embeddings)  # type: ignore[arg-type]
    await diary.search_for_agent("alice-id", query="x", limit=3, include_expired=True)
    sql, _ = db.executed[0]
    # The default branch has ``ttl_until IS NULL OR ttl_until > now()``;
    # the include_expired branch must not have any ttl_until-based
    # filter on the WHERE clause.
    assert "ttl_until IS NULL" not in sql
    assert "ttl_until > now()" not in sql
    assert "embedding IS NOT NULL" in sql


async def test_search_for_agent_swallows_embed_failure() -> None:
    """A transient embeddings outage during search returns ``[]``
    without raising; the personal-memory prefetch falls back to the
    recency half of the hybrid candidate selection."""
    diary = AgentDiary(_DBStub(), _EmbeddingStub(raises=True))  # type: ignore[arg-type]
    out = await diary.search_for_agent("alice-id", query="x")
    assert out == []


async def test_search_for_agent_swallows_db_failure() -> None:
    class _BoomDB:
        async def execute(self, *args: Any, **kwargs: Any) -> Any:
            raise RuntimeError("connection reset")

    diary = AgentDiary(_BoomDB(), _EmbeddingStub())  # type: ignore[arg-type]
    out = await diary.search_for_agent("alice-id", query="x")
    assert out == []
