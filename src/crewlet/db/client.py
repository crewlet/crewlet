"""Database client wrapping an asyncpg connection pool."""

from __future__ import annotations

import asyncio
import time
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Any, Protocol, runtime_checkable

from crewlet._logging import get_logger

logger = get_logger("db.client")

# How long a caller waits for an advisory lock before giving up.
#
# Sized against the thing the lock actually guards: a full migration run
# on a cold database. The heaviest shipped statements are the TimescaleDB
# hypertable conversions in 001/002 (`migrate_data => TRUE`) and the
# pgvector index builds — seconds on a fresh database, low minutes on one
# with a large `crewlet_events` history. Five minutes clears that with
# room to spare, while still failing far inside PostgreSQL's default
# two-hour `tcp_keepalives_idle` — the window in which a partitioned,
# SIGKILLed holder's half-open backend keeps the lock. Waiting longer
# than the reap interval would mean waiting forever for a dead process.
ADVISORY_LOCK_TIMEOUT_SECONDS = 300.0

# Poll interval while waiting. Short enough that a normal handover (one
# process finishing its migrations) costs no perceptible startup delay,
# long enough that a fleet of booting replicas cannot busy-spin the
# database with try-lock statements.
_LOCK_POLL_SECONDS = 0.5

try:
    import asyncpg
except ImportError:  # pragma: no cover
    asyncpg = None  # type: ignore[assignment]


@runtime_checkable
class SQLExecutor(Protocol):
    """The three-method surface every query helper is written against.

    Both :class:`Database` (pool-per-statement) and
    :class:`DatabaseConnection` (one held connection) satisfy it, so a
    store can be handed either without knowing which it got.  Test
    doubles implement the same three methods.
    """

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]: ...

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None: ...

    async def fetchval(self, query: str, *args: Any) -> Any: ...


class DatabaseConnection:
    """A single held asyncpg connection with the :class:`SQLExecutor` surface.

    Handed out by :meth:`Database.acquire` and :meth:`Database.transaction`.
    Every statement runs on the SAME backend connection, which is what
    makes session-scoped state work:

    - **Advisory locks.** ``pg_advisory_lock`` is held by a *session*.
      Taken through :class:`Database`, the pooled connection is returned
      immediately and asyncpg's pool reset runs ``pg_advisory_unlock_all()``
      — so the lock is gone before the guarded work begins.  Through this
      class it survives until the block exits.
    - **Transactions.** Multi-statement atomicity needs one connection;
      see :meth:`Database.transaction`.
    """

    __slots__ = ("_conn",)

    def __init__(self, conn: Any) -> None:
        self._conn = conn

    @property
    def raw(self) -> Any:
        """The underlying asyncpg ``Connection`` (for ``transaction()`` etc.)."""
        return self._conn

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        """Execute a query and return all rows as dicts."""
        rows = await self._conn.fetch(query, *args)
        return [dict(r) for r in rows]

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        """Execute a query and return the first row, or ``None``."""
        row = await self._conn.fetchrow(query, *args)
        return dict(row) if row is not None else None

    async def fetchval(self, query: str, *args: Any) -> Any:
        """Execute a query and return the first column of the first row."""
        return await self._conn.fetchval(query, *args)


class Database:
    """Async database wrapper around an asyncpg connection pool.

    Use the :meth:`connect` class method to create an instance, and
    :meth:`close` to shut it down.

    The three query helpers acquire and release a pooled connection per
    statement, which is right for ordinary reads and writes and wrong for
    anything session-scoped — use :meth:`acquire` / :meth:`transaction`
    for those.
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

    @asynccontextmanager
    async def acquire(self) -> AsyncIterator[DatabaseConnection]:
        """Hold one pooled connection for the duration of the block.

        Required for session-scoped state — advisory locks above all.
        ``await db.execute("SELECT pg_advisory_lock($1)", key)`` on the
        pool path is a silent no-op: the connection returns to the pool
        straight away and asyncpg's reset issues
        ``pg_advisory_unlock_all()``.

        >>> async with db.acquire() as conn:
        ...     await conn.fetchval("SELECT pg_advisory_lock($1)", key)
        ...     ...                      # lock genuinely held here
        ...     await conn.fetchval("SELECT pg_advisory_unlock($1)", key)
        """
        pool = self._require_pool()
        async with pool.acquire() as conn:
            yield DatabaseConnection(conn)

    @asynccontextmanager
    async def transaction(self) -> AsyncIterator[DatabaseConnection]:
        """Run a block inside one database transaction.

        Commits on clean exit, rolls back on any exception.  Use for
        multi-statement writes that must be atomic — a migration file's
        statements plus its ``schema_migrations`` row, a claim plus its
        bookkeeping.
        """
        pool = self._require_pool()
        async with pool.acquire() as conn, conn.transaction():
            yield DatabaseConnection(conn)

    @asynccontextmanager
    async def advisory_lock(
        self, key: int, *, timeout_seconds: float = ADVISORY_LOCK_TIMEOUT_SECONDS
    ) -> AsyncIterator[DatabaseConnection]:
        """Hold a session-scoped PostgreSQL advisory lock for the block.

        Concurrent callers serialize rather than fail.  The connection is
        held for the whole block (see :meth:`acquire`), the lock is
        released on exit, and — the property that actually matters — it
        also releases if this process dies, because the backend drops the
        session.  That is why an advisory lock is right for the migrator
        specifically and wrong nearly everywhere else: a SIGKILLed
        migrator must not wedge every future boot, whereas a long-lived
        owner (a seat, a singleton duty) needs a lease with a TTL and a
        fencing token — see :mod:`crewlet.db.leases`.

        The wait is **bounded and announced**.  A bare
        ``pg_advisory_lock`` waits forever, and the release-on-death
        argument only holds when the OS closes the socket: a migrator
        killed inside a network partition leaves a half-open backend
        holding the lock until PostgreSQL's ``tcp_keepalives_idle``
        (2 hours by default) reaps it.  Every booting process would then
        hang with no output at all.  So: try first, log at INFO before
        waiting, and give up at ``timeout_seconds`` with an error naming
        the blocking backend.
        """
        async with self.acquire() as conn:
            got = await conn.fetchval("SELECT pg_try_advisory_lock($1)", key)
            if not got:
                logger.info(
                    "waiting_for_advisory_lock", key=key, timeout=timeout_seconds
                )
                deadline = time.monotonic() + timeout_seconds
                while not got:
                    if time.monotonic() >= deadline:
                        holders = await conn.fetchval(
                            "SELECT string_agg(pid::text, ',') FROM pg_locks "
                            "WHERE locktype = 'advisory' AND granted",
                        )
                        raise TimeoutError(
                            f"timed out after {timeout_seconds}s waiting for "
                            f"advisory lock {key}; holder backend pid(s): "
                            f"{holders or 'unknown'}"
                        )
                    await asyncio.sleep(_LOCK_POLL_SECONDS)
                    got = await conn.fetchval("SELECT pg_try_advisory_lock($1)", key)
            logger.debug("advisory_lock_acquired", key=key)
            try:
                yield conn
            finally:
                # The unlock must never replace the block's own
                # exception.  A migration that fails because the
                # connection dropped mid-statement would otherwise
                # surface as ``ConnectionDoesNotExistError`` from *this*
                # line, and the operator would never see which file
                # failed or why.  Losing the unlock costs nothing that
                # matters: the lock is session-scoped, so a backend that
                # cannot answer has already released it by dying.
                try:
                    released = await conn.fetchval("SELECT pg_advisory_unlock($1)", key)
                except Exception as exc:
                    logger.debug("advisory_unlock_failed", key=key, error=str(exc))
                else:
                    if not released:
                        # We held it on entry, so a false here means the
                        # session was reset underneath us — worth saying,
                        # since the next waiter's timeout would otherwise
                        # be the first hint.
                        logger.warning("advisory_lock_not_held_at_release", key=key)
                    logger.debug("advisory_lock_released", key=key)
