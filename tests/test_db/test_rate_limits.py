"""Contract suite for the shared notification rate-limit valve."""

from __future__ import annotations

import os
import re
from typing import Any

import pytest

from crewlet.db.rate_limits import MemoryRateLimitStore, PostgresRateLimitStore

_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")


class _FakeSQL:
    """A fake that checks the parameter contract before answering.

    It used to ignore ``query`` entirely, and that is how the shipped
    statement came to reference ``$1`` and ``$3`` while binding three
    arguments. PostgreSQL cannot type an unreferenced parameter, so it
    refused to parse the statement at all — every ``allow`` raised, hit
    the fail-open below it, and returned True. The valve was open on
    every database-backed deployment, and the suite was green.

    So the fake now asserts what a real backend enforces: placeholders
    run ``$1..$n`` with no gaps, and ``n`` equals the number of
    arguments. Cheap, needs no database, and catches the whole class.
    """

    def __init__(self) -> None:
        self.counts: dict[str, int] = {}
        self.fail = False

    @staticmethod
    def _check_params(query: str, args: tuple[Any, ...]) -> None:
        used = {int(n) for n in re.findall(r"\$(\d+)", query)}
        if not used and not args:
            return
        expected = set(range(1, len(args) + 1))
        assert used == expected, (
            f"statement binds {len(args)} argument(s) but references "
            f"{sorted(used)}; PostgreSQL refuses to parse a statement "
            f"whose parameter it cannot type"
        )

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        self._check_params(query, args)
        if self.fail:
            raise RuntimeError("database is down")
        self.counts[args[0]] = self.counts.get(args[0], 0) + 1
        return {"count": self.counts[args[0]]}


def _memory() -> Any:
    return MemoryRateLimitStore()


def _postgres() -> Any:
    return PostgresRateLimitStore(_FakeSQL())


class _RealDB:
    """PostgresRateLimitStore over a real database, one connection per test.

    Per-test because pytest-asyncio gives each test a fresh event loop
    and an asyncpg pool is bound to the loop that made it — the shape
    tests/test_db/test_leases.py explains at length.

    Built lazily on first use so constructing the fixture stays sync,
    and every call goes through ``_ready`` so the table is truthed once
    per test rather than leaking rows between them.
    """

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._db: Any = None
        self._store: Any = None

    async def _ready(self) -> Any:
        if self._store is None:
            from crewlet.db.client import Database

            self._db = await Database.connect(self._dsn)
            await self._db.execute("DELETE FROM rate_limits")
            self._store = PostgresRateLimitStore(self._db)
        return self._store

    def __getattr__(self, name: str) -> Any:
        async def _call(*args: Any, **kwargs: Any) -> Any:
            store = await self._ready()
            return await getattr(store, name)(*args, **kwargs)

        return _call

    async def aclose(self) -> None:
        if self._db is not None:
            await self._db.close()
            self._db = None


@pytest.fixture(
    params=[
        pytest.param(_memory, id="memory"),
        pytest.param(_postgres, id="postgres"),
        pytest.param(
            lambda: _RealDB(_TEST_DSN),
            id="postgres-real",
            marks=[
                pytest.mark.integration,
                pytest.mark.skipif(
                    not _TEST_DSN,
                    reason="set CREWLET_TEST_DSN to run against real PG",
                ),
            ],
        ),
    ]
)
async def store(request: pytest.FixtureRequest) -> Any:
    built = request.param()
    yield built
    if isinstance(built, _RealDB):
        await built.aclose()


async def test_allows_up_to_the_limit_then_refuses(store: Any) -> None:
    for _ in range(3):
        assert await store.allow("notify:a", limit=3, window_seconds=60) is True
    assert await store.allow("notify:a", limit=3, window_seconds=60) is False


async def test_a_zero_limit_is_off(store: Any) -> None:
    for _ in range(50):
        assert await store.allow("notify:a", limit=0, window_seconds=60) is True


async def test_buckets_are_independent(store: Any) -> None:
    assert await store.allow("notify:a", limit=1, window_seconds=60) is True
    assert await store.allow("notify:b", limit=1, window_seconds=60) is True
    assert await store.allow("notify:a", limit=1, window_seconds=60) is False


async def test_two_nodes_share_one_budget() -> None:
    """A notification loop bounces between nodes, so a per-process valve
    never sees enough of it to trip."""
    shared = _memory()
    assert await shared.allow("notify:a", limit=2, window_seconds=60) is True
    assert await shared.allow("notify:a", limit=2, window_seconds=60) is True
    assert await shared.allow("notify:a", limit=2, window_seconds=60) is False


async def test_store_failure_fails_open() -> None:
    """It is a valve, not a gate: an unreachable limiter must not stop
    real notifications."""
    sql = _FakeSQL()
    store = PostgresRateLimitStore(sql)
    sql.fail = True
    assert await store.allow("notify:a", limit=1, window_seconds=60) is True
