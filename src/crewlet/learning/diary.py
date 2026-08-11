"""AgentDiary -- the agent's private observation log.

The ``agent_diary`` table holds two kinds of entries:
``diary_long`` (no expiry) and ``diary_short`` (TTL'd, filtered at
read time).  Naming: ``diary`` not ``memory`` to underscore the
semantic difference -- this is the agent's *private observation
log*, not knowledge other agents can query.

Two writers converge here:

* :class:`~crewlet.learning.persist_decider.PersistDecider` --
  post-turn LLM-backed classifier.
* :func:`~crewlet.learning.tools.register_reflect_and_persist_tool`
  -- in-flight LLM-facing tool.

One reader, two callers:

* :func:`~crewlet.learning.personal_memory.fetch_existing_memories`
  -- powers the ``## Personal memory`` Plan-prompt block AND
  PersistDecider's write-side dedup signal.

The dict shape returned by :meth:`AgentDiary.list_for_agent`:

* ``id``, ``content`` (always)
* ``metadata`` -- a flattened dict that includes ``kind``, ``ttl_until``,
  ``source``, ``turn_id``, and any extra row-blob keys.  The flat
  shape lets ``meta.get('kind')`` callers stay simple.

The diary's *write* surface is structured columns; the *read* shape
is a flattened metadata-blob, so the same dict shape works whether
a caller pulls from ``list_for_agent`` or ``search_for_agent``.
"""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any, Literal
from uuid import UUID, uuid4

from crewlet._logging import get_logger
from crewlet.db._jsonb import decode_jsonb_dict
from crewlet.db.client import Database
from crewlet.providers.embeddings.protocol import EmbeddingProvider

logger = get_logger("learning.diary")

DiaryKind = Literal["diary_long", "diary_short"]
"""Discrete tier values stored in the ``agent_diary.kind`` column."""

KIND_LONG: DiaryKind = "diary_long"
KIND_SHORT: DiaryKind = "diary_short"
DIARY_KINDS = frozenset({KIND_LONG, KIND_SHORT})

# ----------------------------------------------------------------------
# Write-boundary hygiene
# ----------------------------------------------------------------------
#
# Two cheap, uncontroversial guards run on every diary write: a per-row
# content cap and an exact-duplicate check.  Prompt-injection scanning
# at this boundary is intentionally *not* here -- a deliberate design
# call: regex detection is brittle and false-positives on legitimate
# reflections, so it does not belong in a hygiene pass.

# Embedding-input cap.  The stored ``content`` is NEVER truncated --
# the agent must read back exactly what was written.  This cap applies
# ONLY to the text handed to the embeddings provider, which has a hard
# token limit; the slice is used to compute the similarity vector, not
# to bound what we persist.  Generous enough that short declarative
# facts (what ``reflect_and_persist`` / PersistDecider produce) embed
# in full.
_MAX_EMBED_INPUT_CHARS = 8000


def _vector_literal(vec: list[float]) -> str:
    """Encode a float list as a pgvector literal (``[a,b,...]``)."""
    return "[" + ",".join(str(v) for v in vec) + "]"


class AgentDiary:
    """Postgres-backed per-agent diary store.

    Composite identity: rows are keyed by ``agent_id`` (the
    deterministic UUID derived from ``(org.name, handle)``) so a
    renamed handle cleanly orphans old rows rather than mixing them
    with the new identity.

    All public methods are best-effort -- failures log and return
    safe defaults rather than raising up to the writer / reader.
    Personal memory must never break a turn.

    ``embeddings`` is optional: when wired, every write embeds the
    content so :meth:`search_for_agent` can do vector similarity
    against the agent's diary.  This powers the ``## Personal
    memory`` Plan-prompt prefetch's hybrid candidate selection
    (vector top-K union recency top-K) so old-but-relevant memories
    can reach the aux relevance filter even when they fall outside
    the recency window.  Without an embedding provider (test /
    in-memory mode) rows land with ``embedding=NULL`` and
    :meth:`search_for_agent` returns ``[]``; the prefetch falls back
    to pure recency.
    """

    def __init__(
        self,
        db: Database,
        embeddings: EmbeddingProvider | None = None,
    ) -> None:
        self._db = db
        self._embeddings = embeddings

    # ------------------------------------------------------------------
    # Write
    # ------------------------------------------------------------------

    async def write(
        self,
        *,
        agent_id: str,
        kind: DiaryKind,
        content: str,
        ttl_until: datetime | None = None,
        source: str = "",
        turn_id: str = "",
        metadata: dict[str, Any] | None = None,
    ) -> str:
        """Insert one diary row, return the row id.

        Embeds the content when an ``EmbeddingProvider`` was wired so
        :meth:`search_for_agent` can rank by similarity.  Without an
        embedding provider the row lands with ``embedding=NULL``;
        vector search excludes it but :meth:`list_for_agent` still
        surfaces it via the recency half of the prefetch's hybrid
        candidate selection.

        Embedding failures are swallowed -- the content still gets
        persisted with NULL embedding so a transient embeddings-API
        outage doesn't cause memory loss.

        Write-boundary hygiene: content is stored in full (never
        length-truncated) and deduplicated against the agent's live
        rows (an exact match returns the existing row id without a
        second insert).  Only the text sent to the embeddings provider
        is sliced to ``_MAX_EMBED_INPUT_CHARS`` to stay within the
        provider's token limit.  Prompt-injection scanning is
        deliberately *not* done here (see the write-boundary hygiene
        note above).

        Raises :class:`ValueError` for invalid inputs (empty agent_id,
        unknown kind).  DB exceptions propagate -- callers wrap with
        their own try/except (PersistDecider already does).
        """
        if not agent_id:
            raise ValueError("agent_id is required")
        if kind not in DIARY_KINDS:
            raise ValueError(f"kind must be one of {sorted(DIARY_KINDS)}")
        if not content:
            raise ValueError("content is required")
        # Exact-duplicate guard.  Both writers (PersistDecider post-turn
        # and the reflect_and_persist builtin in-flight) can land the
        # same fact; the read-side aux filter is probabilistic, so
        # dedupe deterministically here against the agent's live rows.
        existing_id = await self._find_duplicate(agent_id, content)
        if existing_id is not None:
            logger.debug("agent_diary_dedup_hit", agent_id=agent_id, doc_id=existing_id)
            return existing_id
        doc_id = uuid4()
        embedding_literal: str | None = None
        if self._embeddings is not None:
            try:
                # Slice ONLY the embedding input to the provider's token
                # limit; the full content is still stored below.
                vectors = await self._embeddings.embed(
                    [content[:_MAX_EMBED_INPUT_CHARS]]
                )
                if vectors:
                    embedding_literal = _vector_literal(vectors[0])
            except Exception:
                logger.exception(
                    "agent_diary_embed_failed",
                    agent_id=agent_id,
                    kind=kind,
                )
        await self._db.execute(
            """
            INSERT INTO agent_diary (
                id, agent_id, kind, content, ttl_until,
                source, turn_id, metadata, embedding
            )
            VALUES (
                $1::uuid, $2, $3, $4, $5, $6, $7, $8::jsonb,
                $9::vector
            )
            """,
            str(doc_id),
            agent_id,
            kind,
            content,
            ttl_until,
            source,
            turn_id,
            _encode_metadata(metadata),
            embedding_literal,
        )
        logger.debug(
            "agent_diary_stored",
            doc_id=str(doc_id),
            agent_id=agent_id,
            kind=kind,
            ttl_until=ttl_until.isoformat() if ttl_until is not None else "",
            embedded=embedding_literal is not None,
        )
        return str(doc_id)

    async def _find_duplicate(self, agent_id: str, content: str) -> str | None:
        """Return the id of a live row with identical content, if any.

        Scoped to non-expired rows -- an expired SHORT entry with the
        same text should not block a fresh write (the stale one is on
        its way out).  Best-effort: a check failure logs and returns
        ``None`` so the write proceeds rather than being lost.
        """
        try:
            rows = await self._db.execute(
                """
                SELECT id FROM agent_diary
                WHERE agent_id = $1
                  AND content = $2
                  AND (ttl_until IS NULL OR ttl_until > now())
                LIMIT 1
                """,
                agent_id,
                content,
            )
        except Exception:
            logger.exception("agent_diary_dedup_check_failed", agent_id=agent_id)
            return None
        if rows:
            return str(rows[0].get("id") or "") or None
        return None

    # ------------------------------------------------------------------
    # Read
    # ------------------------------------------------------------------

    async def list_for_agent(
        self,
        agent_id: str,
        *,
        limit: int = 100,
        include_expired: bool = False,
    ) -> list[dict[str, Any]]:
        """Return diary rows for ``agent_id``, newest-first.

        TTL filtering (``include_expired=False``, the default) drops
        any ``diary_short`` row whose ``ttl_until`` is in the past --
        ``diary_long`` rows always pass.  Expired rows aren't deleted
        by this read; they accumulate until :meth:`delete_expired`
        runs (today best-effort, future scheduled cleanup).

        The returned dict shape: ``content`` plus a ``metadata``
        blob that flattens ``kind`` / ``ttl_until`` / ``source`` /
        ``turn_id`` into the same dict alongside any original row
        metadata.  This lets ``personal_memory.py`` and
        ``persist_decider.py`` consumers read from a uniform
        ``meta.get('kind')`` shape regardless of which method
        produced the dict.
        """
        if not agent_id:
            return []
        try:
            rows = await self._db.execute(
                """
                SELECT id, agent_id, kind, content, ttl_until,
                       source, turn_id, metadata,
                       retrieval_count, last_retrieved_at, created_at
                FROM agent_diary
                WHERE agent_id = $1
                ORDER BY created_at DESC
                LIMIT $2
                """,
                agent_id,
                int(limit),
            )
        except Exception:
            logger.exception("agent_diary_list_failed", agent_id=agent_id)
            return []
        now = datetime.now(UTC)
        out: list[dict[str, Any]] = []
        for row in rows:
            if not include_expired and _row_expired(row, now=now):
                continue
            out.append(_row_to_doc(row))
        return out

    async def search_for_agent(
        self,
        agent_id: str,
        *,
        query: str,
        limit: int = 50,
        include_expired: bool = False,
        min_similarity: float = 0.0,
    ) -> list[dict[str, Any]]:
        """Vector similarity over the agent's own diary.

        Powers the personal-memory prefetch's hybrid candidate
        selection: vector top-K (semantic match on the turn's trigger)
        is unioned with recency top-K (broadly-applicable rules that
        may not topical-match) and deduped before the aux relevance
        filter sees the merged pool.

        Returns rows shaped like :meth:`list_for_agent`, plus a
        ``similarity`` field.  Rows without an embedding (e.g. written
        before an ``EmbeddingProvider`` was wired, or persisted
        through a transient embeddings outage) are excluded by the
        partial HNSW index -- the recency half of the hybrid still
        surfaces them via :meth:`list_for_agent`.

        ``min_similarity`` is a relevance floor on the raw cosine
        similarity: rows below it are dropped.  Vector search always
        returns the nearest ``limit`` rows however far away they
        are; the floor lets the caller honestly return "no semantic
        match" instead of the agent's least-irrelevant entries.
        ``0.0`` (the default) disables the floor.

        Returns ``[]`` when:
        - ``agent_id`` or ``query`` is blank,
        - no embedding provider is wired,
        - the embeddings call fails,
        - the DB query fails (logged, swallowed).
        """
        if not agent_id or not query:
            return []
        if self._embeddings is None:
            return []
        try:
            vectors = await self._embeddings.embed([query])
        except Exception:
            logger.exception("agent_diary_search_embed_failed", agent_id=agent_id)
            return []
        if not vectors:
            return []
        embedding_literal = _vector_literal(vectors[0])
        try:
            if include_expired:
                rows = await self._db.execute(
                    """
                    SELECT id, agent_id, kind, content, ttl_until,
                           source, turn_id, metadata,
                           retrieval_count, last_retrieved_at, created_at,
                           1 - (embedding <=> $1::vector) AS similarity
                    FROM agent_diary
                    WHERE agent_id = $2
                      AND embedding IS NOT NULL
                    ORDER BY similarity DESC
                    LIMIT $3
                    """,
                    embedding_literal,
                    agent_id,
                    int(limit),
                )
            else:
                rows = await self._db.execute(
                    """
                    SELECT id, agent_id, kind, content, ttl_until,
                           source, turn_id, metadata,
                           retrieval_count, last_retrieved_at, created_at,
                           1 - (embedding <=> $1::vector) AS similarity
                    FROM agent_diary
                    WHERE agent_id = $2
                      AND embedding IS NOT NULL
                      AND (ttl_until IS NULL OR ttl_until > now())
                    ORDER BY similarity DESC
                    LIMIT $3
                    """,
                    embedding_literal,
                    agent_id,
                    int(limit),
                )
        except Exception:
            logger.exception("agent_diary_search_failed", agent_id=agent_id)
            return []
        results = [_row_to_doc_with_similarity(row) for row in rows]
        if min_similarity > 0.0:
            results = [
                d
                for d in results
                if float(d.get("similarity") or 0.0) >= min_similarity
            ]
        return results

    async def fetch_by_id(self, doc_id: str | UUID) -> dict[str, Any] | None:
        """Return one row by id, or ``None`` when missing."""
        if not doc_id:
            return None
        try:
            rows = await self._db.execute(
                """
                SELECT id, agent_id, kind, content, ttl_until,
                       source, turn_id, metadata,
                       retrieval_count, last_retrieved_at, created_at
                FROM agent_diary
                WHERE id = $1::uuid
                """,
                str(doc_id),
            )
        except Exception:
            logger.exception("agent_diary_fetch_failed", doc_id=str(doc_id))
            return None
        if not rows:
            return None
        return _row_to_doc(rows[0])

    async def count_for_agent(
        self, agent_id: str, *, include_expired: bool = False
    ) -> int:
        """Cheap count for an agent, used by tests / dashboards."""
        if not agent_id:
            return 0
        try:
            if include_expired:
                rows = await self._db.execute(
                    """
                    SELECT COUNT(*)::int AS n FROM agent_diary
                    WHERE agent_id = $1
                    """,
                    agent_id,
                )
            else:
                rows = await self._db.execute(
                    """
                    SELECT COUNT(*)::int AS n FROM agent_diary
                    WHERE agent_id = $1
                      AND (ttl_until IS NULL OR ttl_until > now())
                    """,
                    agent_id,
                )
        except Exception:
            logger.exception("agent_diary_count_failed", agent_id=agent_id)
            return 0
        if not rows:
            return 0
        return int(rows[0].get("n") or 0)

    # ------------------------------------------------------------------
    # Mutations on existing rows
    # ------------------------------------------------------------------

    async def mark_retrieved(self, doc_id: str | UUID) -> None:
        """Bump ``retrieval_count`` and stamp ``last_retrieved_at``.

        Called after the personal-memory aux-LLM filter selects a row
        for the Plan-prompt block.  Best-effort: a UPDATE failure is
        logged but never surfaced (telemetry never breaks a turn).
        """
        if not doc_id:
            return
        try:
            await self._db.execute(
                """
                UPDATE agent_diary
                SET retrieval_count = retrieval_count + 1,
                    last_retrieved_at = now()
                WHERE id = $1::uuid
                """,
                str(doc_id),
            )
        except Exception:
            logger.exception("agent_diary_mark_retrieved_failed", doc_id=str(doc_id))

    async def mark_retrieved_many(self, doc_ids: list[str | UUID]) -> None:
        """Batched :meth:`mark_retrieved` -- one UPDATE for N rows.

        The personal-memory prefetch selects several rows per turn;
        bumping them one ``await`` at a time put N serial round-trips
        on the Plan-phase critical path.  This collapses them into a
        single ``WHERE id = ANY($1)`` statement.  Best-effort.
        """
        ids = [str(d) for d in doc_ids if d]
        if not ids:
            return
        try:
            await self._db.execute(
                """
                UPDATE agent_diary
                SET retrieval_count = retrieval_count + 1,
                    last_retrieved_at = now()
                WHERE id = ANY($1::uuid[])
                """,
                ids,
            )
        except Exception:
            logger.exception("agent_diary_mark_retrieved_many_failed", count=len(ids))

    async def delete(self, doc_id: str | UUID) -> bool:
        """Remove one row by id.  Returns ``True`` when a row was deleted."""
        if not doc_id:
            return False
        try:
            rows = await self._db.execute(
                """
                DELETE FROM agent_diary
                WHERE id = $1::uuid
                RETURNING id
                """,
                str(doc_id),
            )
        except Exception:
            logger.exception("agent_diary_delete_failed", doc_id=str(doc_id))
            return False
        return bool(rows)

    async def delete_expired(self) -> int:
        """Drop all ``diary_short`` rows whose TTL is in the past.

        Returns the number of rows deleted.  Idempotent.  Not
        scheduled today; called manually / by tests.  A future
        operational task may call this from a daily cron when the
        diary grows large enough to need it.
        """
        try:
            rows = await self._db.execute(
                """
                DELETE FROM agent_diary
                WHERE ttl_until IS NOT NULL
                  AND ttl_until < now()
                RETURNING id
                """,
            )
        except Exception:
            logger.exception("agent_diary_delete_expired_failed")
            return 0
        deleted = len(rows)
        if deleted:
            logger.info("agent_diary_expired_pruned", count=deleted)
        return deleted


# ----------------------------------------------------------------------
# Encoding helpers
# ----------------------------------------------------------------------


def _encode_metadata(metadata: dict[str, Any] | None) -> str:
    """Encode the JSONB metadata blob.

    ``None`` becomes the empty object literal so the column's
    ``NOT NULL DEFAULT '{}'::jsonb`` constraint is satisfied without
    a separate code path.
    """
    import json as _json

    return _json.dumps(metadata if metadata is not None else {})


def _row_expired(row: dict[str, Any], *, now: datetime | None = None) -> bool:
    """Compare ``ttl_until`` against ``now``; ``None`` TTL never expires.

    Fails *closed*: a ``ttl_until`` that can't be parsed into a
    datetime is treated as expired.  A row whose expiry is corrupt is
    safer hidden than shown forever -- the alternative would re-inject
    an unbounded stale entry into every Plan prompt.
    """
    ttl = row.get("ttl_until")
    if ttl is None:
        return False
    if isinstance(ttl, str):
        try:
            ttl = datetime.fromisoformat(ttl)
        except ValueError:
            return True
    if not isinstance(ttl, datetime):
        return True
    if ttl.tzinfo is None:
        ttl = ttl.replace(tzinfo=UTC)
    return ttl < (now or datetime.now(UTC))


def _row_to_doc_with_similarity(row: dict[str, Any]) -> dict[str, Any]:
    """Like :func:`_row_to_doc` but propagates the ``similarity``
    column produced by :meth:`AgentDiary.search_for_agent`."""
    doc = _row_to_doc(row)
    if "similarity" in row:
        doc["similarity"] = row["similarity"]
    return doc


def _row_to_doc(row: dict[str, Any]) -> dict[str, Any]:
    """Translate a row dict into the memory-doc shape consumers read.

    ``personal_memory.py`` and ``persist_decider.py`` consumers read
    ``doc['content']`` and ``doc['metadata']['kind']`` /
    ``['ttl_until']`` -- so we flatten the structured columns back
    into a unified ``metadata`` dict alongside the JSONB blob.
    """
    metadata = decode_jsonb_dict(row.get("metadata"))
    metadata["kind"] = row.get("kind") or ""
    source = row.get("source") or ""
    if source:
        metadata["source"] = source
    turn_id = row.get("turn_id") or ""
    if turn_id:
        metadata["turn_id"] = turn_id
    ttl = row.get("ttl_until")
    if ttl is not None:
        if isinstance(ttl, datetime):
            metadata["ttl_until"] = ttl.isoformat()
        else:
            metadata["ttl_until"] = str(ttl)
    return {
        "id": str(row.get("id") or ""),
        "agent_id": row.get("agent_id") or "",
        "content": row.get("content") or "",
        "metadata": metadata,
        "retrieval_count": int(row.get("retrieval_count") or 0),
        "last_retrieved_at": row.get("last_retrieved_at"),
        "created_at": row.get("created_at"),
    }
