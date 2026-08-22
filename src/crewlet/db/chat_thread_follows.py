"""Typed queries against the ``chat_thread_follows`` table.

Backend-neutral: one repository serves every chat transport, keyed by
``backend`` so a Mattermost root-post id and a Slack ``thread_ts`` can
never satisfy each other's lookups (they are drawn from different
namespaces and are not guaranteed distinct).
"""

from __future__ import annotations

from crewlet.db.client import Database

# How long a thread-follow survives with no activity.
#
# ``updated_at`` is refreshed on EVERY re-assert — a mention, a
# collective address, the agent posting into the thread — so it is a
# true last-activity stamp rather than a creation date.  Ninety days is
# the point past which a chat thread has stopped being a live
# conversation on every backend that ships one: Slack and Mattermost
# both surface a quarter-old thread only by search.
#
# The asymmetry decides the value.  Dropping a stale follow costs at
# most one missed NON-mention reply, and the very next mention re-follows
# through the ordinary path — while keeping every follow forever costs
# unbounded growth on a table read on the hot path of every inbound chat
# message.  A cheap, self-healing miss beats an unbounded read.
FOLLOW_RETENTION_SECONDS = 90 * 24 * 3600.0


class ChatThreadFollowRepository:
    """Persist per-agent chat thread-follow state across engine restarts."""

    def __init__(self, db: Database) -> None:
        self._db = db

    async def upsert(
        self,
        backend: str,
        handle: str,
        channel: str,
        thread_id: str,
        reason: str,
    ) -> None:
        """Insert or update a thread-follow entry."""
        await self._db.execute(
            """
            INSERT INTO chat_thread_follows
                (backend, agent_handle, channel_id, thread_id, reason, updated_at)
            VALUES ($1, $2, $3, $4, $5, now())
            ON CONFLICT (backend, agent_handle, channel_id, thread_id) DO UPDATE
            SET reason     = EXCLUDED.reason,
                updated_at = EXCLUDED.updated_at
            """,
            backend,
            handle,
            channel,
            thread_id,
            reason,
        )

    async def is_following(
        self, backend: str, handle: str, channel: str, thread_id: str
    ) -> str | None:
        """Return the follow reason if the agent is following, else ``None``."""
        return await self._db.fetchval(
            """
            SELECT reason FROM chat_thread_follows
            WHERE backend = $1
              AND agent_handle = $2
              AND channel_id = $3
              AND thread_id = $4
            """,
            backend,
            handle,
            channel,
            thread_id,
        )

    async def purge(self, older_than_seconds: float) -> int:
        """Delete follows untouched for ``older_than_seconds``.

        The :class:`~crewlet.db.maintenance.PurgeableStore` contract, so
        the maintenance worker can sweep this table like any other.
        """
        rows = await self._db.execute(
            """
            DELETE FROM chat_thread_follows
            WHERE updated_at < now() - make_interval(secs => $1)
            RETURNING thread_id
            """,
            float(older_than_seconds),
        )
        return len(rows)


__all__ = ["FOLLOW_RETENTION_SECONDS", "ChatThreadFollowRepository"]
