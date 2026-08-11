"""Database client wrapping an asyncpg connection pool."""

from __future__ import annotations

from typing import Any

from crewlet._logging import get_logger

logger = get_logger("db.client")

try:
    import asyncpg
except ImportError:  # pragma: no cover
    asyncpg = None  # type: ignore[assignment]


class Database:
    """Async database wrapper around an asyncpg connection pool.

    Use the :meth:`connect` class method to create an instance, and
    :meth:`close` to shut it down.
    """

    def __init__(self) -> None:
        self._pool: asyncpg.Pool | None = None  # type: ignore[name-defined]

    @classmethod
    async def connect(cls, dsn: str) -> Database:
        """Connect to a PostgreSQL database and return a :class:`Database`."""
        if asyncpg is None:
            msg = (
                "asyncpg is required for PostgreSQL connections"
                " — install crewlet[postgresql]"
            )
            raise RuntimeError(msg)
        db = cls()
        db._pool = await asyncpg.create_pool(dsn)
        logger.info("database_connected")
        return db

    async def close(self) -> None:
        """Close the connection pool."""
        if self._pool is not None:
            await self._pool.close()
            self._pool = None
            logger.info("database_closed")

    def _require_pool(self) -> asyncpg.Pool:  # type: ignore[name-defined]
        """Return the connection pool, raising if not connected."""
        if self._pool is None:
            raise RuntimeError("Database is not connected")
        return self._pool

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        """Execute a query and return all rows as dicts."""
        pool = self._require_pool()
        rows = await pool.fetch(query, *args)
        return [dict(r) for r in rows]

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        """Execute a query and return the first row, or ``None``."""
        pool = self._require_pool()
        row = await pool.fetchrow(query, *args)
        return dict(row) if row is not None else None

    async def fetchval(self, query: str, *args: Any) -> Any:
        """Execute a query and return the first column of the first row."""
        pool = self._require_pool()
        return await pool.fetchval(query, *args)
