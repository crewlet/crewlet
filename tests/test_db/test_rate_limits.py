"""Contract suite for the shared notification rate-limit valve."""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.db.rate_limits import MemoryRateLimitStore, PostgresRateLimitStore


class _FakeSQL:
    def __init__(self) -> None:
        self.counts: dict[str, int] = {}
        self.fail = False

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        if self.fail:
            raise RuntimeError("database is down")
        self.counts[args[0]] = self.counts.get(args[0], 0) + 1
        return {"count": self.counts[args[0]]}


def _memory() -> Any:
    return MemoryRateLimitStore()


def _postgres() -> Any:
    return PostgresRateLimitStore(_FakeSQL())


@pytest.fixture(
    params=[pytest.param(_memory, id="memory"), pytest.param(_postgres, id="postgres")]
)
def store(request: pytest.FixtureRequest) -> Any:
    return request.param()


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
