"""EpisodeStore — persistence + similarity search for agent episodes.

Mirrors the shape of :class:`~crewlet.knowledge.providers.pgvector.PgvectorProvider`:
a thin wrapper over the shared :class:`~crewlet.db.client.Database`
pool plus an :class:`~crewlet.providers.embeddings.protocol.EmbeddingProvider`
for vectorisation.

Two physical row shapes share this table (distinguished by ``kind``):

- ``kind="raw"``: the original behaviour, one row per completed turn.
- ``kind="compacted"``: cluster summary written by
  :class:`~crewlet.learning.episode_lifecycle.EpisodeLifecycleWorker`;
  ``count`` is the number of original episodes the row replaces.

Read methods take an optional ``kinds`` filter so callers can pick
which physical shapes they want.  Skill synthesis filters to
``["raw"]`` (compacted is too aggregated to draft a clean skill);
``query_episodes`` and the Plan-prompt prefetch take both, branching
on ``kind`` at render time.

The write path also drives episode lifecycle: after each ``write``,
the store cheaply checks the agent's raw-episode count against
``max_raw_episodes_per_agent`` and -- when the threshold is crossed
-- publishes a ``CompactionRequested`` event.  An
``EpisodeLifecycleWorker`` subscriber then drains that agent's pool
asynchronously.  No daily cron; no caller-path latency on reads;
demand-proportional cost.
"""

from __future__ import annotations

import hashlib
import json
from typing import Any, Protocol

from crewlet._logging import get_logger
from crewlet.db._jsonb import decode_jsonb_str_list, decode_jsonb_uuid_list
from crewlet.db.client import Database
from crewlet.events.types import CompactionRequested
from crewlet.learning.models import Episode
from crewlet.providers.embeddings.protocol import EmbeddingProvider

logger = get_logger("learning.episode_store")

# Default cap; the engine can override via
# ``learning.episode_lifecycle.max_raw_episodes_per_agent``.  Set to
# 0 to disable threshold-triggered compaction entirely (rare; only for
# tests / single-shot usage where no learning loop exists).
DEFAULT_MAX_RAW_EPISODES_PER_AGENT = 500

# How often to actually run the count(*) check.  One cheap SQL per
# write would be fine but unnecessary; checking every Nth keeps the
# write path snappy under heavy traffic.
DEFAULT_WRITE_CHECK_EVERY_N = 10


# The row, written two ways.  ``INSERT … SELECT`` rather than ``INSERT …
# VALUES`` so the guarded form can carry a ``WHERE``; the unguarded form
# keeps the same shape so the two cannot drift in their column lists,
# which is the way a pair of hand-maintained INSERTs always fails.
_EPISODE_COLUMNS = """
    id, agent_handle, agent_role, task_id, turn_id,
    started_at, ended_at, plan_summary, task_summary,
    tool_sequence, skills_used, review_outcome,
    duration_ms, embedding,
    kind, count, exemplar_turn_ids, consolidated_into_skill_id,
    common_task_pattern, common_outcome, success_rate,
    subjects_involved, notable_patterns, work_key
"""

_EPISODE_VALUES = """
    $1::uuid, $2, $3, $4, $5::uuid,
    $6, $7, $8, $9,
    $10::jsonb, $11::jsonb, $12,
    $13, $14::vector,
    $15, $16, $17::jsonb, $18,
    $19, $20, $21,
    $22::jsonb, $23, $24
"""

_INSERT_EPISODE_SQL = f"""
    INSERT INTO episodes ({_EPISODE_COLUMNS})
    SELECT {_EPISODE_VALUES}
"""

# ``RETURNING`` is what tells the caller whether the row landed: with
# ``WHERE NOT EXISTS`` a skipped insert is a successful statement that
# wrote nothing, not an error.
_INSERT_EPISODE_GUARDED_SQL = f"""
    INSERT INTO episodes ({_EPISODE_COLUMNS})
    SELECT {_EPISODE_VALUES}
    WHERE NOT EXISTS (
        SELECT 1 FROM episodes WHERE agent_handle = $2 AND work_key = $24
    )
    RETURNING id
"""


def _insert_args(episode: Episode, embedding_literal: str | None) -> tuple[Any, ...]:
    """Positional arguments for both INSERT forms, in column order.

    One builder for both so the ``$n`` placeholders and the values
    cannot drift apart -- the failure mode of a duplicated INSERT is a
    column silently written with its neighbour's value.
    """
    return (
        str(episode.id),
        episode.agent_handle,
        episode.agent_role,
        episode.task_id,
        str(episode.turn_id),
        episode.started_at,
        episode.ended_at,
        episode.plan_summary,
        episode.task_summary,
        json.dumps(episode.tool_sequence),
        json.dumps(episode.skills_used),
        episode.review_outcome,
        episode.duration_ms,
        embedding_literal,
        episode.kind,
        episode.count,
        json.dumps([str(x) for x in episode.exemplar_turn_ids]),
        (
            str(episode.consolidated_into_skill_id)
            if episode.consolidated_into_skill_id
            else None
        ),
        episode.common_task_pattern,
        episode.common_outcome,
        episode.success_rate,
        json.dumps(episode.subjects_involved),
        episode.notable_patterns,
        episode.work_key,
    )


def _lock_word(agent_handle: str, work_key: str) -> int:
    """A signed 32-bit advisory-lock word for one agent's work key.

    A hash collision costs a moment of needless serialization between
    two unrelated episode writes and nothing else -- the ``WHERE NOT
    EXISTS`` beneath it compares the real values.
    """
    digest = hashlib.sha256(f"{agent_handle}\n{work_key}".encode()).digest()
    return int.from_bytes(digest[:4], "big", signed=True)


class EpisodeStoreProtocol(Protocol):
    """Protocol satisfied by both the real store and test fakes."""

    async def write(self, episode: Episode) -> None: ...

    async def query(
        self,
        *,
        agent_handle: str,
        query_text: str,
        limit: int = 5,
        outcome_filter: str | None = None,
        kinds: list[str] | None = None,
    ) -> list[Episode]: ...

    async def list_recent_by_outcome(
        self,
        *,
        agent_handle: str,
        outcome: str,
        window_hours: int = 168,
        limit: int = 200,
        kinds: list[str] | None = None,
    ) -> list[Episode]: ...

    async def fetch_by_turn_id(self, turn_id: str) -> Episode | None: ...

    async def count_raw_for_agent(self, agent_handle: str) -> int: ...

    async def mark_consolidated(
        self, *, episode_ids: list[str], skill_id: str
    ) -> int: ...

    async def delete_non_terminal_older_than_days(
        self, *, agent_handle: str, days: int
    ) -> int: ...

    async def delete_consolidated_older_than_days(
        self, *, agent_handle: str, days: int
    ) -> int: ...

    async def delete_compacted_older_than_days(
        self, *, agent_handle: str, days: int
    ) -> int: ...

    async def fetch_raw_for_compaction(
        self, *, agent_handle: str, older_than_days: int, limit: int
    ) -> list[Episode]: ...

    async def insert_compacted(self, episode: Episode) -> None: ...

    async def delete_episodes_by_ids(self, ids: list[str]) -> int: ...


class EpisodeStore:
    """Postgres/pgvector-backed episode store."""

    def __init__(
        self,
        db: Database,
        embeddings: EmbeddingProvider,
        *,
        event_queue: Any = None,
        max_raw_episodes_per_agent: int = DEFAULT_MAX_RAW_EPISODES_PER_AGENT,
        write_check_every_n: int = DEFAULT_WRITE_CHECK_EVERY_N,
    ) -> None:
        self._db = db
        self._embeddings = embeddings
        self._event_queue = event_queue
        self._threshold = max(0, int(max_raw_episodes_per_agent))
        self._check_every = max(1, int(write_check_every_n))
        # Per-agent write counter -- avoids a count(*) on every single
        # write under heavy traffic.  Bounded growth: only as many
        # entries as there are distinct agent handles.
        self._writes_since_check: dict[str, int] = {}

    # ------------------------------------------------------------------
    # Write
    # ------------------------------------------------------------------

    async def _embed_or_none(self, text: str) -> str | None:
        """Embed ``text`` into a pgvector literal, or ``None`` on failure.

        Mirrors ``AgentDiary``: a transient embeddings-API outage must
        not lose the episode.  The row still lands with
        ``embedding=NULL`` -- vector recall skips it (the partial HNSW
        index excludes NULLs) while the time-window / outcome queries
        still surface it.
        """
        try:
            vectors = await self._embeddings.embed([text])
        except Exception:
            logger.exception("episode_embed_failed")
            return None
        if not vectors:
            return None
        return _vector_literal(vectors[0])

    # PostgreSQL keeps the two-argument advisory-lock space separate from
    # the one-argument (bigint) space, so a key here can never collide
    # with the migrator's lock however the second word hashes.
    _WORK_KEY_LOCK_NAMESPACE = 0x4350  # "CP" — crewlet episodes

    async def _insert(self, episode: Episode, embedding_literal: str | None) -> bool:
        """Write the row.  ``False`` when this work is already recorded.

        Two paths, and the split is the point:

        * **No work key** -- a turn with no ledgerable trigger (a
          scheduled fire, a sub-agent, a sandbox resume).  There is no
          cross-node duplicate to collapse, so it takes the plain insert
          and pays nothing for a guard it cannot use.
        * **A work key** -- a transaction-scoped advisory lock on
          ``(agent_handle, work_key)``, then ``INSERT … WHERE NOT
          EXISTS``.

        The lock is what makes it exact.  ``WHERE NOT EXISTS`` on its own
        is evaluated under the statement's snapshot, so two concurrent
        transactions can both see "not there" and both insert -- the
        classic phantom.  Serializing on the key closes it, and a
        transaction-scoped lock releases on commit with no unlock
        bookkeeping and no way to outlive a killed process.

        A unique index would be the obvious answer and is not available
        here: ``episodes`` is a hypertable partitioned on ``ended_at``,
        so every unique index must contain ``ended_at`` -- and the two
        duplicate turns end at different times.  See
        ``migrations/031_work_key.sql``.
        """
        if not episode.work_key:
            await self._db.execute(
                _INSERT_EPISODE_SQL, *_insert_args(episode, embedding_literal)
            )
            return True
        async with self._db.transaction() as conn:
            await conn.execute(
                "SELECT pg_advisory_xact_lock($1::int, $2::int)",
                self._WORK_KEY_LOCK_NAMESPACE,
                _lock_word(episode.agent_handle, episode.work_key),
            )
            rows = await conn.execute(
                _INSERT_EPISODE_GUARDED_SQL,
                *_insert_args(episode, embedding_literal),
            )
        return bool(rows)

    async def write(self, episode: Episode) -> None:
        """Insert one raw episode, embedding its task+plan summary.

        After write, periodically checks the agent's raw-episode count;
        when it crosses ``max_raw_episodes_per_agent`` a
        ``CompactionRequested`` event is published so the
        :class:`EpisodeLifecycleWorker` can drain the pool.

        Failures are logged and re-raised; the caller (TurnEngine)
        wraps this in a try/except so episode write failures never
        break turn completion.

        Embedding failures are *not* re-raised: like ``AgentDiary``,
        the row still lands with ``embedding=NULL`` so a transient
        embeddings-API outage doesn't lose the episode -- vector recall
        skips it (the partial HNSW index excludes NULLs) while the
        time-window / outcome queries still surface it.

        **Idempotent per ``work_key``.**  Two nodes that both complete a
        turn for one trigger write one row, not two -- see
        :meth:`_insert`.  A duplicated episode is the least visible harm
        this design admits to: a doubled Slack post is obvious and
        recoverable, while a doubled memory is retrieved by every later
        turn's episode recall and feeds skill synthesis.
        """
        embedding_literal = await self._embed_or_none(episode.embeddable_text)
        if not await self._insert(episode, embedding_literal):
            # Another node already recorded this unit of work. INFO
            # rather than a warning: this is the guard doing its job, on
            # exactly the paths designed to be able to produce a second
            # turn for one trigger.
            logger.info(
                "episode_duplicate_skipped",
                agent_handle=episode.agent_handle,
                turn_id=str(episode.turn_id),
                work_key=episode.work_key,
            )
            return
        logger.info(
            "episode_stored",
            agent_handle=episode.agent_handle,
            turn_id=str(episode.turn_id),
            review_outcome=episode.review_outcome,
            tool_count=len(episode.tool_sequence),
            kind=episode.kind,
        )
        # Lifecycle trigger: only fires for raw rows (compacted writes
        # come from the worker itself; would otherwise loop).
        if episode.kind == "raw":
            await self._maybe_trigger_compaction(episode.agent_handle)

    async def insert_compacted(self, episode: Episode) -> None:
        """Insert a pre-built ``kind='compacted'`` episode.

        Used by :class:`EpisodeLifecycleWorker` after it has clustered
        and LLM-summarised a batch of raw episodes.  Doesn't fire the
        compaction trigger (would loop).
        """
        if episode.kind != "compacted":
            raise ValueError(
                f"insert_compacted requires kind='compacted', got {episode.kind!r}"
            )
        embedding_literal = await self._embed_or_none(episode.embeddable_text)
        await self._db.execute(
            """
            INSERT INTO episodes (
                id, agent_handle, agent_role, task_id, turn_id,
                started_at, ended_at, plan_summary, task_summary,
                tool_sequence, skills_used, review_outcome,
                duration_ms, embedding,
                kind, count, exemplar_turn_ids, consolidated_into_skill_id,
                common_task_pattern, common_outcome, success_rate,
                subjects_involved, notable_patterns
            )
            VALUES (
                $1::uuid, $2, $3, $4, $5::uuid,
                $6, $7, $8, $9,
                $10::jsonb, $11::jsonb, $12,
                $13, $14::vector,
                $15, $16, $17::jsonb, $18,
                $19, $20, $21,
                $22::jsonb, $23
            )
            """,
            str(episode.id),
            episode.agent_handle,
            episode.agent_role,
            episode.task_id,
            str(episode.turn_id),
            episode.started_at,
            episode.ended_at,
            episode.plan_summary,
            episode.task_summary,
            json.dumps(episode.tool_sequence),
            json.dumps(episode.skills_used),
            episode.review_outcome,
            episode.duration_ms,
            embedding_literal,
            episode.kind,
            episode.count,
            json.dumps([str(x) for x in episode.exemplar_turn_ids]),
            None,  # compacted rows are never themselves consolidated
            episode.common_task_pattern,
            episode.common_outcome,
            episode.success_rate,
            json.dumps(episode.subjects_involved),
            episode.notable_patterns,
        )
        logger.info(
            "compacted_episode_stored",
            agent_handle=episode.agent_handle,
            count=episode.count,
            common_outcome=episode.common_outcome,
            exemplar_count=len(episode.exemplar_turn_ids),
        )

    async def _maybe_trigger_compaction(self, agent_handle: str) -> None:
        """Increment per-agent write counter and check threshold.

        Best-effort: a publish failure must not surface back to the
        write path -- episodes still got written, lifecycle can catch
        up on the next trigger.
        """
        if self._threshold <= 0 or self._event_queue is None or not agent_handle:
            return
        pending = self._writes_since_check.get(agent_handle, 0) + 1
        if pending < self._check_every:
            self._writes_since_check[agent_handle] = pending
            return
        # Reset before the count call so we don't double-fire when
        # counts oscillate around the threshold.
        self._writes_since_check[agent_handle] = 0
        try:
            raw_count = await self.count_raw_for_agent(agent_handle)
        except Exception:
            logger.exception(
                "episode_lifecycle_count_failed", agent_handle=agent_handle
            )
            return
        if raw_count <= self._threshold:
            return
        try:
            await self._event_queue.publish(
                "crewlet.events.compaction_requested",
                CompactionRequested(
                    source="episode_store",
                    agent_handle=agent_handle,
                    raw_count=raw_count,
                    threshold=self._threshold,
                ),
            )
            logger.info(
                "compaction_requested",
                agent_handle=agent_handle,
                raw_count=raw_count,
                threshold=self._threshold,
            )
        except Exception:
            logger.exception(
                "compaction_request_publish_failed", agent_handle=agent_handle
            )

    # ------------------------------------------------------------------
    # Read
    # ------------------------------------------------------------------

    async def query(
        self,
        *,
        agent_handle: str,
        query_text: str,
        limit: int = 5,
        outcome_filter: str | None = None,
        kinds: list[str] | None = None,
    ) -> list[Episode]:
        """Return episodes most similar to ``query_text`` for this agent.

        Always scoped to a single ``agent_handle``.  ``outcome_filter``
        restricts to one review outcome (e.g. ``"done"``) when the
        caller only wants successful prior work.  ``kinds`` defaults
        to ``["raw", "compacted"]`` -- both physical shapes -- so
        ``query_episodes`` recall surfaces aggregate patterns
        alongside individual turns.
        """
        kinds = list(kinds) if kinds else ["raw", "compacted"]
        embedding_literal = await self._embed_or_none(query_text)
        if embedding_literal is None:
            # No usable query embedding (transient embeddings outage) --
            # vector recall can't run; return nothing rather than a
            # broken ORDER BY.
            return []
        # ``embedding IS NOT NULL`` so rows written during an embed
        # outage (NULL embedding) are skipped instead of sorting on a
        # NULL similarity.
        clauses = ["agent_handle = $2", "kind = ANY($3)", "embedding IS NOT NULL"]
        params: list[Any] = [embedding_literal, agent_handle, kinds]
        if outcome_filter is not None:
            clauses.append(f"review_outcome = ${len(params) + 1}")
            params.append(outcome_filter)
        params.append(limit)
        sql = f"""
            SELECT id, agent_handle, agent_role, task_id, turn_id,
                   started_at, ended_at, plan_summary, task_summary,
                   tool_sequence, skills_used, review_outcome,
                   duration_ms,
                   kind, count, exemplar_turn_ids,
                   consolidated_into_skill_id,
                   common_task_pattern, common_outcome, success_rate,
                   subjects_involved, notable_patterns,
                   1 - (embedding <=> $1::vector) AS similarity
            FROM episodes
            WHERE {" AND ".join(clauses)}
            ORDER BY similarity DESC
            LIMIT ${len(params)}
        """
        rows = await self._db.execute(sql, *params)
        logger.debug(
            "episodes_queried",
            agent_handle=agent_handle,
            returned=len(rows),
            outcome_filter=outcome_filter or "",
            kinds=kinds,
        )
        return [_row_to_episode(row) for row in rows]

    async def list_recent_by_outcome(
        self,
        *,
        agent_handle: str,
        outcome: str,
        window_hours: int = 168,
        limit: int = 200,
        kinds: list[str] | None = None,
    ) -> list[Episode]:
        """Return this agent's recent episodes with the given outcome.

        Defaults to ``kinds=["raw"]`` -- the skill synthesizer (the
        primary caller) wants raw episodes only because the compacted
        aggregate is too coarse to draft a clean skill from.  Operators
        querying for analytics can pass both.
        """
        if not agent_handle or not outcome:
            return []
        kinds = list(kinds) if kinds else ["raw"]
        rows = await self._db.execute(
            """
            SELECT id, agent_handle, agent_role, task_id, turn_id,
                   started_at, ended_at, plan_summary, task_summary,
                   tool_sequence, skills_used, review_outcome,
                   duration_ms,
                   kind, count, exemplar_turn_ids,
                   consolidated_into_skill_id,
                   common_task_pattern, common_outcome, success_rate,
                   subjects_involved, notable_patterns
            FROM episodes
            WHERE agent_handle = $1
              AND review_outcome = $2
              AND ended_at >= now() - make_interval(hours => $3::int)
              AND kind = ANY($4)
            ORDER BY ended_at DESC
            LIMIT $5
            """,
            agent_handle,
            outcome,
            int(window_hours),
            kinds,
            int(limit),
        )
        logger.debug(
            "episodes_listed_recent",
            agent_handle=agent_handle,
            outcome=outcome,
            returned=len(rows),
            window_hours=window_hours,
            kinds=kinds,
        )
        return [_row_to_episode(row) for row in rows]

    async def fetch_by_turn_id(self, turn_id: str) -> Episode | None:
        """Return the episode written for the given ``turn_id``, or None."""
        if not turn_id:
            return None
        rows = await self._db.execute(
            """
            SELECT id, agent_handle, agent_role, task_id, turn_id,
                   started_at, ended_at, plan_summary, task_summary,
                   tool_sequence, skills_used, review_outcome,
                   duration_ms,
                   kind, count, exemplar_turn_ids,
                   consolidated_into_skill_id,
                   common_task_pattern, common_outcome, success_rate,
                   subjects_involved, notable_patterns
            FROM episodes
            WHERE turn_id = $1::uuid
            ORDER BY ended_at DESC
            LIMIT 1
            """,
            turn_id,
        )
        if not rows:
            return None
        return _row_to_episode(rows[0])

    async def count_raw_for_agent(self, agent_handle: str) -> int:
        """Count raw rows for one agent.  Used by the lifecycle trigger
        and by the worker to decide whether compaction is needed."""
        if not agent_handle:
            return 0
        rows = await self._db.execute(
            """
            SELECT count(*) AS c
            FROM episodes
            WHERE agent_handle = $1
              AND kind = 'raw'
            """,
            agent_handle,
        )
        if not rows:
            return 0
        value = rows[0].get("c", 0)
        return int(value) if value is not None else 0

    # ------------------------------------------------------------------
    # Lifecycle: targeted deletes + compaction-pool fetch
    # ------------------------------------------------------------------

    async def mark_consolidated(self, *, episode_ids: list[str], skill_id: str) -> int:
        """Stamp ``consolidated_into_skill_id`` on each given episode.

        Called by :class:`SkillSynthesizer` after a successful skill
        write.  The lifecycle worker later drops these episodes after
        the configured grace -- the skill carries the learning.

        Idempotent: subsequent calls with the same skill_id are a
        no-op (the column is already set).
        """
        if not episode_ids or not skill_id:
            return 0
        await self._db.execute(
            """
            UPDATE episodes
               SET consolidated_into_skill_id = $1::uuid
             WHERE id = ANY($2::uuid[])
               AND consolidated_into_skill_id IS NULL
            """,
            skill_id,
            episode_ids,
        )
        logger.debug(
            "episodes_marked_consolidated",
            skill_id=skill_id,
            count=len(episode_ids),
        )
        return len(episode_ids)

    async def delete_non_terminal_older_than_days(
        self, *, agent_handle: str, days: int
    ) -> int:
        """Drop ``self_iterate`` rows older than N days.

        This mid-state outcome doesn't feed skill synthesis (the reflect
        engine's terminal-outcome gate filters it out) and only feeds
        ``query_episodes`` recall as noise.  Drop fast.
        """
        if not agent_handle or days <= 0:
            return 0
        rows = await self._db.execute(
            """
            DELETE FROM episodes
             WHERE agent_handle = $1
               AND kind = 'raw'
               AND review_outcome = 'self_iterate'
               AND ended_at < now() - make_interval(days => $2::int)
             RETURNING 1
            """,
            agent_handle,
            int(days),
        )
        return len(rows)

    async def delete_consolidated_older_than_days(
        self, *, agent_handle: str, days: int
    ) -> int:
        """Drop episodes that were consolidated into a skill ≥ ``days`` ago.

        Once the synthesizer has captured the learning in a skill, the
        raw episode is redundant for future planning.  The grace
        window gives a chance to audit / detect bad consolidations
        before the source disappears.
        """
        if not agent_handle or days <= 0:
            return 0
        rows = await self._db.execute(
            """
            DELETE FROM episodes
             WHERE agent_handle = $1
               AND kind = 'raw'
               AND consolidated_into_skill_id IS NOT NULL
               AND ended_at < now() - make_interval(days => $2::int)
             RETURNING 1
            """,
            agent_handle,
            int(days),
        )
        return len(rows)

    async def delete_compacted_older_than_days(
        self, *, agent_handle: str, days: int
    ) -> int:
        """Evict ``kind='compacted'`` rows older than N days.

        Optional very-long-tail eviction; default-disabled (days=0).
        Useful for orgs that want hard storage caps over years of data.
        """
        if not agent_handle or days <= 0:
            return 0
        rows = await self._db.execute(
            """
            DELETE FROM episodes
             WHERE agent_handle = $1
               AND kind = 'compacted'
               AND ended_at < now() - make_interval(days => $2::int)
             RETURNING 1
            """,
            agent_handle,
            int(days),
        )
        return len(rows)

    async def fetch_raw_for_compaction(
        self,
        *,
        agent_handle: str,
        older_than_days: int,
        limit: int,
    ) -> list[Episode]:
        """Return raw, non-consolidated episodes older than the grace
        window, oldest-first, ready for clustering + compaction.

        Excludes:
        - rows already marked ``consolidated_into_skill_id`` (about to
          be dropped by the consolidation cleanup anyway)
        - rows in the recent ``older_than_days`` window (still inside
          the active learning windows used by the skill synthesizer
          and by ``## Similar prior work`` recall)
        - ``self_iterate`` (handled by the non-terminal cleanup;
          should never get here)
        """
        if not agent_handle:
            return []
        rows = await self._db.execute(
            """
            SELECT id, agent_handle, agent_role, task_id, turn_id,
                   started_at, ended_at, plan_summary, task_summary,
                   tool_sequence, skills_used, review_outcome,
                   duration_ms,
                   kind, count, exemplar_turn_ids,
                   consolidated_into_skill_id,
                   common_task_pattern, common_outcome, success_rate,
                   subjects_involved, notable_patterns
            FROM episodes
            WHERE agent_handle = $1
              AND kind = 'raw'
              AND consolidated_into_skill_id IS NULL
              AND review_outcome IN ('done', 'failed')
              AND ended_at < now() - make_interval(days => $2::int)
            ORDER BY ended_at ASC
            LIMIT $3
            """,
            agent_handle,
            int(older_than_days),
            int(limit),
        )
        return [_row_to_episode(row) for row in rows]

    async def delete_episodes_by_ids(self, ids: list[str]) -> int:
        """Bulk-delete by ids.  Used by the lifecycle worker after
        a cluster has been compacted into a single row."""
        if not ids:
            return 0
        rows = await self._db.execute(
            """
            DELETE FROM episodes WHERE id = ANY($1::uuid[])
            RETURNING 1
            """,
            ids,
        )
        return len(rows)


def _vector_literal(vec: list[float]) -> str:
    """Convert a float list to a pgvector literal like ``[0.1,0.2]``."""
    return "[" + ",".join(str(v) for v in vec) + "]"


def _row_to_episode(row: dict) -> Episode:
    return Episode(
        id=row["id"],
        # ``.get``: no read path selects this today, and one that starts
        # to should not have to remember to plumb it. A field written and
        # never read back is how a model quietly stops describing its
        # own table.
        work_key=row.get("work_key") or "",
        agent_handle=row["agent_handle"],
        agent_role=row["agent_role"],
        task_id=row.get("task_id") or "",
        turn_id=row["turn_id"],
        started_at=row["started_at"],
        ended_at=row["ended_at"],
        plan_summary=row["plan_summary"],
        task_summary=row["task_summary"],
        tool_sequence=decode_jsonb_str_list(row.get("tool_sequence")),
        skills_used=decode_jsonb_str_list(row.get("skills_used")),
        review_outcome=row["review_outcome"],
        duration_ms=row["duration_ms"],
        kind=row.get("kind") or "raw",
        count=int(row.get("count") or 1),
        exemplar_turn_ids=decode_jsonb_uuid_list(row.get("exemplar_turn_ids")),
        consolidated_into_skill_id=row.get("consolidated_into_skill_id"),
        common_task_pattern=row.get("common_task_pattern") or "",
        common_outcome=row.get("common_outcome") or "",
        success_rate=float(row.get("success_rate") or 0.0),
        subjects_involved=decode_jsonb_str_list(row.get("subjects_involved")),
        notable_patterns=row.get("notable_patterns") or "",
    )
