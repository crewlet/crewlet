"""Contract suite for shared token-budget usage.

Runs against the PostgreSQL store (through a SQL-executing fake) and the
memory twin, so a divergence is a failing test rather than a
production-only surprise.
"""

from __future__ import annotations

import os
from typing import Any

import pytest

from crewlet.concurrency import BudgetManager
from crewlet.db.budgets import (
    ORG_SCOPE,
    MemoryBudgetUsageStore,
    PostgresBudgetUsageStore,
    agent_scope,
)

_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")


class _FakeSQL:
    """Applies the store's SQL semantically, including the ON CONFLICT
    WHERE guard that makes check-and-increment one step, and transaction
    rollback (which is what un-charges the org when a seat is refused)."""

    def __init__(self) -> None:
        self.rows: dict[str, int] = {}

    def _bump(self, scope: str, delta: int, limit: int) -> dict[str, Any] | None:
        current = self.rows.get(scope)
        if current is None:
            self.rows[scope] = delta
            return {"used_tokens": delta}
        if limit and current + delta > limit:
            return None  # ON CONFLICT ... WHERE rejected the update
        self.rows[scope] = current + delta
        return {"used_tokens": self.rows[scope]}

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        q = " ".join(query.split())
        if q.startswith("INSERT INTO token_budget_usage"):
            return self._bump(args[0], int(args[1]), int(args[2]))
        if q.startswith("SELECT used_tokens"):
            used = self.rows.get(args[0])
            return {"used_tokens": used} if used is not None else None
        raise AssertionError(f"unexpected fetchrow: {q}")

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        q = " ".join(query.split())
        if q.startswith("SELECT used_tokens"):
            used = self.rows.get(args[0])
            return [{"used_tokens": used}] if used is not None else []
        if q.startswith("DELETE FROM token_budget_usage WHERE scope"):
            return [{"scope": args[0]}] if self.rows.pop(args[0], None) else []
        if q.startswith("DELETE FROM token_budget_usage"):
            out = [{"scope": s} for s in self.rows]
            self.rows.clear()
            return out
        raise AssertionError(f"unexpected execute: {q}")

    async def fetchval(self, query: str, *args: Any) -> Any:
        raise AssertionError("unused")


class _FakeDB:
    """Database stand-in whose ``transaction()`` really rolls back."""

    def __init__(self) -> None:
        self.sql = _FakeSQL()

    def transaction(self) -> Any:
        import contextlib

        sql = self.sql

        @contextlib.asynccontextmanager
        async def _tx() -> Any:
            snapshot = dict(sql.rows)
            try:
                yield sql
            except BaseException:
                sql.rows.clear()
                sql.rows.update(snapshot)
                raise

        return _tx()

    async def fetchrow(self, query: str, *args: Any) -> Any:
        return await self.sql.fetchrow(query, *args)

    async def execute(self, query: str, *args: Any) -> Any:
        return await self.sql.execute(query, *args)


def _memory() -> Any:
    return MemoryBudgetUsageStore()


def _postgres() -> Any:
    return PostgresBudgetUsageStore(_FakeDB())


class _RealDB:
    """PostgresBudgetUsageStore over a real database, one connection per test.

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
            await self._db.execute("DELETE FROM token_budget_usage")
            self._store = PostgresBudgetUsageStore(self._db)
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


async def test_spend_within_limits_records_usage(store: Any) -> None:
    out = await store.spend(agent_id="a1", tokens=100, org_limit=1000, agent_limit=500)
    assert out.ok
    assert out.org_used == 100
    assert out.agent_used == 100
    assert await store.usage(ORG_SCOPE) == 100
    assert await store.usage(agent_scope("a1")) == 100


async def test_unlimited_is_zero(store: Any) -> None:
    out = await store.spend(agent_id="a1", tokens=10**9, org_limit=0, agent_limit=0)
    assert out.ok


async def test_org_cap_refuses_and_names_itself(store: Any) -> None:
    await store.spend(agent_id="a1", tokens=900, org_limit=1000, agent_limit=0)
    out = await store.spend(agent_id="a1", tokens=200, org_limit=1000, agent_limit=0)
    assert not out.ok
    assert out.rejected_scope == "org"
    assert out.rejected_limit == 1000


async def test_agent_cap_refuses_and_names_itself(store: Any) -> None:
    out = await store.spend(agent_id="a1", tokens=600, org_limit=0, agent_limit=500)
    assert not out.ok
    assert out.rejected_scope == "agent"
    assert out.rejected_limit == 500


async def test_a_refused_seat_does_not_charge_the_org(store: Any) -> None:
    """The reason org and agent move in one transaction: a turn refused at
    the seat never ran, so the org must not be billed for it."""
    await store.spend(agent_id="a1", tokens=100, org_limit=10_000, agent_limit=400)
    before = await store.usage(ORG_SCOPE)

    out = await store.spend(
        agent_id="a1", tokens=900, org_limit=10_000, agent_limit=400
    )

    assert not out.ok and out.rejected_scope == "agent"
    assert await store.usage(ORG_SCOPE) == before


async def test_spend_larger_than_the_cap_is_refused_on_a_fresh_scope(
    store: Any,
) -> None:
    """The upsert's INSERT branch has no existing value to test against,
    so an over-cap first spend has to be screened before writing."""
    out = await store.spend(
        agent_id="fresh", tokens=5000, org_limit=1000, agent_limit=0
    )
    assert not out.ok
    assert out.rejected_scope == "org"
    assert await store.usage(ORG_SCOPE) == 0


async def test_exact_cap_is_allowed(store: Any) -> None:
    out = await store.spend(agent_id="a1", tokens=1000, org_limit=1000, agent_limit=0)
    assert out.ok


async def test_zero_and_negative_spends_are_noops(store: Any) -> None:
    assert (await store.spend(agent_id="a1", tokens=0, org_limit=1, agent_limit=1)).ok
    assert await store.usage(ORG_SCOPE) == 0


async def test_reset_clears_one_scope_or_all(store: Any) -> None:
    await store.spend(agent_id="a1", tokens=10, org_limit=0, agent_limit=0)
    await store.spend(agent_id="a2", tokens=10, org_limit=0, agent_limit=0)

    assert await store.reset(agent_scope("a1")) == 1
    assert await store.usage(agent_scope("a1")) == 0
    assert await store.usage(agent_scope("a2")) == 10

    assert await store.reset() >= 1
    assert await store.usage(ORG_SCOPE) == 0


# --- the manager façade ----------------------------------------------------


async def test_manager_enforces_through_the_store() -> None:
    manager = BudgetManager(org_budget=1000, usage_store=_memory())
    assert (await manager.spend("a1", 600)).ok
    refused = await manager.spend("a1", 600)
    assert not refused.ok
    assert refused.rejected_scope == "org"


async def test_manager_mirrors_usage_for_advisory_reads() -> None:
    """Sub-agent slice sizing and the reflect engine's exhausted-check read
    the mirror; it must track this process's own spends."""
    manager = BudgetManager(org_budget=1000, usage_store=_memory())
    manager.set_agent_budget("a1", 500)
    await manager.spend("a1", 300)

    assert manager.org_budget.used_tokens == 300
    assert manager.get_agent_budget("a1").used_tokens == 300


async def test_manager_refresh_pulls_a_peers_spend_into_the_mirror() -> None:
    """The mirror only moves on this process's spends, so a node that has
    been idle lags its peers until it refreshes."""
    shared = _memory()
    node_a = BudgetManager(org_budget=1000, usage_store=shared)
    node_b = BudgetManager(org_budget=1000, usage_store=shared)
    node_b.set_agent_budget("a1", 900)

    await node_a.spend("a1", 400)
    assert node_b.org_budget.used_tokens == 0  # hasn't looked yet

    await node_b.refresh_mirror("a1")
    assert node_b.org_budget.used_tokens == 400
    assert node_b.get_agent_budget("a1").used_tokens == 400


async def test_two_managers_sharing_a_store_enforce_one_org_cap() -> None:
    """The whole point. With per-process counters an org cap of 1000
    became 2000 across two engines, silently."""
    shared = _memory()
    node_a = BudgetManager(org_budget=1000, usage_store=shared)
    node_b = BudgetManager(org_budget=1000, usage_store=shared)

    assert (await node_a.spend("a1", 700)).ok
    refused = await node_b.spend("a2", 700)

    assert not refused.ok
    assert refused.rejected_scope == "org"


async def test_consume_keeps_its_boolean_contract() -> None:
    manager = BudgetManager(org_budget=100, usage_store=_memory())
    assert await manager.consume("a1", 50) is True
    assert await manager.consume("a1", 100) is False
