"""Contract suite for inbound-delivery dedupe.

Runs against the PostgreSQL store (through a SQL-executing fake) and the
memory twin, so a divergence is a failing test rather than a
production-only surprise.
"""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.db.deliveries import (
    MemoryDeliveryDedupeStore,
    PostgresDeliveryDedupeStore,
)


class _FakeSQL:
    """Applies the store's SQL semantically, including the ON CONFLICT
    DO NOTHING that makes the claim first-wins in one statement."""

    def __init__(self) -> None:
        self.rows: set[tuple[str, str]] = set()
        self.fail = False

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        if self.fail:
            raise RuntimeError("database is down")
        entry = (args[0], args[1])
        if entry in self.rows:
            return None
        self.rows.add(entry)
        return {"delivery_key": args[1]}

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        if self.fail:
            raise RuntimeError("database is down")
        out = [{"delivery_key": k} for _s, k in self.rows]
        self.rows.clear()
        return out


def _memory() -> Any:
    return MemoryDeliveryDedupeStore()


def _postgres() -> Any:
    return PostgresDeliveryDedupeStore(_FakeSQL())


@pytest.fixture(
    params=[pytest.param(_memory, id="memory"), pytest.param(_postgres, id="postgres")]
)
def store(request: pytest.FixtureRequest) -> Any:
    return request.param()


async def test_first_claim_wins(store: Any) -> None:
    assert await store.claim("github", "delivery-1") is True
    assert await store.claim("github", "delivery-1") is False


async def test_sources_are_namespaced(store: Any) -> None:
    """Two integrations must not collide on a shared key space — a Jira
    timestamp and a Slack ts could otherwise be the same string."""
    assert await store.claim("jira", "same-key") is True
    assert await store.claim("slack", "same-key") is True


async def test_distinct_keys_both_claim(store: Any) -> None:
    assert await store.claim("github", "a") is True
    assert await store.claim("github", "b") is True


async def test_an_empty_key_always_claims(store: Any) -> None:
    """A source with no stable identity to dedupe on must still be
    handled: a missed duplicate is a doubled reply, a wrongly-dropped
    delivery is a message nobody ever answers."""
    assert await store.claim("weird", "") is True
    assert await store.claim("weird", "") is True


async def test_two_nodes_sharing_a_store_dedupe_a_retry() -> None:
    """The whole point. Per-process rings made a provider retry that
    landed on a peer a fresh delivery there, so the agent woke twice and
    could answer twice."""
    shared = _memory()
    assert await shared.claim("github", "delivery-9") is True
    # ...the provider retries and the load balancer picks the other node.
    assert await shared.claim("github", "delivery-9") is False


async def test_postgres_store_fails_open() -> None:
    """An unreachable dedupe store must not stop inbound work."""
    sql = _FakeSQL()
    store = PostgresDeliveryDedupeStore(sql)
    sql.fail = True
    assert await store.claim("github", "delivery-1") is True


async def test_memory_store_expires_claims() -> None:
    import time

    store = MemoryDeliveryDedupeStore(ttl_seconds=0.05)
    assert await store.claim("slack", "k") is True
    assert await store.claim("slack", "k") is False
    time.sleep(0.06)
    assert await store.claim("slack", "k") is True


async def test_purge_drops_old_entries() -> None:
    store = MemoryDeliveryDedupeStore()
    await store.claim("slack", "k")
    assert await store.purge(older_than_seconds=-1) == 1
    assert await store.claim("slack", "k") is True
