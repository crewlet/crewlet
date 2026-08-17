"""Typed queries against the ``chat_thread_follows`` table.

Backend-neutral: one repository serves every chat transport, keyed by
``backend`` so a Mattermost root-post id and a Slack ``thread_ts`` can
never satisfy each other's lookups (they are drawn from different
namespaces and are not guaranteed distinct).
"""

from __future__ import annotations

from crewlet.db.client import Database


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
