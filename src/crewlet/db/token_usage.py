"""Typed queries against the ``token_usage`` table."""

from __future__ import annotations

from crewlet._logging import get_logger
from crewlet.db.client import Database

logger = get_logger("db.token_usage")


class TokenUsageRepository:
    """Track per-agent token consumption."""

    def __init__(self, db: Database) -> None:
        self._db = db

    async def get(self, handle: str) -> int:
        """Return ``tokens_used`` for *handle*, or ``0`` if not found."""
        val = await self._db.fetchval(
            "SELECT tokens_used FROM token_usage WHERE agent_handle = $1",
            handle,
        )
        return val if val is not None else 0

    async def increment(self, handle: str, tokens: int) -> None:
        """Add *tokens* to *handle*'s running total (upsert)."""
        await self._db.execute(
            """
            INSERT INTO token_usage (agent_handle, tokens_used, updated_at)
            VALUES ($1, $2, now())
            ON CONFLICT (agent_handle) DO UPDATE
            SET tokens_used = token_usage.tokens_used + EXCLUDED.tokens_used,
                updated_at  = EXCLUDED.updated_at
            """,
            handle,
            tokens,
        )
        logger.debug("token_usage_incremented", handle=handle, tokens=tokens)

    async def get_all(self) -> dict[str, int]:
        """Return ``{handle: tokens_used}`` for every tracked agent."""
        rows = await self._db.execute(
            "SELECT agent_handle, tokens_used FROM token_usage ORDER BY agent_handle"
        )
        return {row["agent_handle"]: row["tokens_used"] for row in rows}
