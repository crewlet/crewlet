"""Typed queries against the ``slack_thread_follows`` table."""

from __future__ import annotations

from crewlet.db.client import Database


class SlackThreadFollowRepository:
    """Persist Slack thread-follow state across engine restarts."""

    def __init__(self, db: Database) -> None:
        self._db = db

    async def upsert(
        self,
        handle: str,
        channel: str,
        thread_ts: str,
        reason: str,
    ) -> None:
        """Insert or update a thread-follow entry."""
        await self._db.execute(
            """
            INSERT INTO slack_thread_follows
                (agent_handle, channel_id, thread_ts, reason, updated_at)
            VALUES ($1, $2, $3, $4, now())
            ON CONFLICT (agent_handle, channel_id, thread_ts) DO UPDATE
            SET reason     = EXCLUDED.reason,
                updated_at = EXCLUDED.updated_at
            """,
            handle,
            channel,
            thread_ts,
            reason,
        )

    async def is_following(
        self, handle: str, channel: str, thread_ts: str
    ) -> str | None:
        """Return the follow reason if the agent is following, else ``None``."""
        return await self._db.fetchval(
            """
            SELECT reason FROM slack_thread_follows
            WHERE agent_handle = $1 AND channel_id = $2 AND thread_ts = $3
            """,
            handle,
            channel,
            thread_ts,
        )
