"""Contract suite for inbound-delivery dedupe.

Runs against the PostgreSQL store (through a SQL-executing fake), the
memory twin, and — when ``CREWLET_TEST_DSN`` is set — a real database, so
a divergence is a failing test rather than a production-only surprise.

The TTL used to be tested on the memory twin ONLY, and the two backends
had quietly diverged underneath that gap: the Postgres claim was a bare
``ON CONFLICT DO NOTHING`` with no time predicate, so a delivery key was
claimed **forever** while the twin released it after five minutes. An
operator replaying a webhook ten minutes later watched it vanish. Expiry
is a contract test now, on every backend.
"""

from __future__ import annotations

import asyncio
import os
from typing import Any

import pytest

from crewlet.db.deliveries import (
    MemoryDeliveryDedupeStore,
    PostgresDeliveryDedupeStore,
)

_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")

# The TTL is per backend, because "make a claim look old" is a different
# operation on each one and only ONE of them has to do it by waiting.
#
# The memory twin reads ``time.monotonic`` directly, so aging means
# sleeping and the TTL has to be small. The other two age rows without a
# clock — the fake advances a counter, the real one back-dates
# ``seen_at`` — so their TTL is large, and it MUST be: a 50 ms TTL on a
# real database is shorter than two round-trips, so a claim expires
# between the two statements of a test that meant them to be adjacent.
# That is not a hypothetical; it is what this suite failed on first run,
# and it is exactly the class of thing the fake cannot show you.
_MEMORY_TTL = 0.05
_VIRTUAL_TTL = 60.0


class _FakeSQL:
    """Applies the store's SQL semantically, including the conflict
    clause's time predicate — which is what makes the claim expire."""

    def __init__(self) -> None:
        self.rows: dict[tuple[str, str], float] = {}
        self.fail = False
        self.now = 0.0

    def advance(self, seconds: float) -> None:
        self.now += seconds

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        if self.fail:
            raise RuntimeError("database is down")
        source, key, ttl = str(args[0]), str(args[1]), float(args[2])
        seen = self.rows.get((source, key))
        if seen is not None and seen >= self.now - ttl:
            return None
        self.rows[(source, key)] = self.now
        return {"delivery_key": key}

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        if self.fail:
            raise RuntimeError("database is down")
        cutoff = self.now - float(args[0])
        stale = [k for k, seen in self.rows.items() if seen < cutoff]
        for key in stale:
            del self.rows[key]
        return [{"delivery_key": k[1]} for k in stale]


class _RealSQL:
    """The store's SQL against a REAL PostgreSQL, when one is offered.

    The fake can only confirm the SQL means what its author thought it
    meant. A conflict clause carrying a ``WHERE`` on the conflicting
    row's own table alias is exactly the shape that is easy to get subtly
    wrong and impossible to catch unrun.
    """

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._db: Any = None

    async def _pool(self) -> Any:
        if self._db is None:
            from crewlet.db.client import Database

            self._db = await Database.connect(self._dsn)
            await self._db.execute("DELETE FROM webhook_deliveries")
        return self._db

    async def aclose(self) -> None:
        if self._db is not None:
            await self._db.close()
            self._db = None

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        return await (await self._pool()).fetchrow(query, *args)

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        return await (await self._pool()).execute(query, *args)

    async def advance_async(self, seconds: float) -> None:
        """Age every row instead of sleeping."""
        await (await self._pool()).execute(
            "UPDATE webhook_deliveries "
            "SET seen_at = seen_at - make_interval(secs => $1)",
            float(seconds),
        )


class _Backend:
    """One store plus the two things a TTL test needs from it: how long
    the window is, and how to move a claim into the past."""

    def __init__(self, store: Any, ttl: float) -> None:
        self.store = store
        self.ttl = ttl

    async def claim(self, source: str, key: str) -> bool:
        return await self.store.claim(source, key)

    async def purge(self, older_than_seconds: float) -> int:
        return await self.store.purge(older_than_seconds)

    async def age(self, ttl_multiples: float) -> None:
        seconds = self.ttl * ttl_multiples
        inner = getattr(self.store, "_db", None)
        if inner is None:  # the memory twin reads a real clock
            await asyncio.sleep(seconds)
        elif isinstance(inner, _RealSQL):
            await inner.advance_async(seconds)
        else:
            inner.advance(seconds)

    async def aclose(self) -> None:
        inner = getattr(self.store, "_db", None)
        if isinstance(inner, _RealSQL):
            await inner.aclose()


def _memory() -> _Backend:
    return _Backend(MemoryDeliveryDedupeStore(ttl_seconds=_MEMORY_TTL), _MEMORY_TTL)


def _postgres() -> _Backend:
    return _Backend(
        PostgresDeliveryDedupeStore(_FakeSQL(), ttl_seconds=_VIRTUAL_TTL), _VIRTUAL_TTL
    )


def _real_postgres() -> _Backend:
    return _Backend(
        PostgresDeliveryDedupeStore(_RealSQL(_TEST_DSN), ttl_seconds=_VIRTUAL_TTL),
        _VIRTUAL_TTL,
    )


BACKENDS = [
    pytest.param(_memory, id="memory"),
    pytest.param(_postgres, id="postgres"),
    pytest.param(
        _real_postgres,
        id="postgres-real",
        marks=[
            pytest.mark.integration,
            pytest.mark.skipif(
                not _TEST_DSN, reason="set CREWLET_TEST_DSN to run against real PG"
            ),
        ],
    ),
]


@pytest.fixture(params=BACKENDS)
async def store(request: pytest.FixtureRequest) -> Any:
    built = request.param()
    yield built
    await built.aclose()


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


async def test_a_claim_expires_after_the_ttl(store: Any) -> None:
    """The property that had diverged.

    The claim answers "did I just see this", not "did I ever see this".
    A permanent claim silently swallows an operator's replay and any
    genuinely new delivery that happens to reuse a key.
    """
    assert await store.claim("slack", "k") is True
    assert await store.claim("slack", "k") is False
    await store.age(1.5)
    assert await store.claim("slack", "k") is True


async def test_a_claim_inside_the_ttl_still_refuses(store: Any) -> None:
    """The other half — expiry must not be so eager that a provider's
    own immediate retry gets through."""
    assert await store.claim("slack", "k") is True
    await store.age(0.25)
    assert await store.claim("slack", "k") is False


async def test_purge_never_drops_a_live_claim(store: Any) -> None:
    """Garbage collection, not expiry.

    ``claim`` already enforces the TTL, so a sweep exists only to stop
    the table growing — and it must never reach a claim still inside its
    window, or the sweep itself becomes a periodic hole in the dedupe.

    Asserted as behaviour rather than a row count on purpose: the memory
    twin prunes itself inline as a side effect of ``claim``, so *how
    many* rows a given purge finds is a backend detail. What must hold
    everywhere is that the live claim is still refused afterwards.
    """
    assert await store.claim("slack", "old") is True
    await store.age(4)
    assert await store.claim("slack", "fresh") is True

    await store.purge(older_than_seconds=store.ttl * 2)
    assert await store.claim("slack", "fresh") is False


async def test_purge_actually_deletes_rows() -> None:
    """The growth this exists to stop.

    On the memory twin an uncollected entry is a dict key; on Postgres it
    is a row in a table written to on every inbound webhook, and both
    migrations promise a sweep that never existed. Counted here, where
    the backend has no inline prune to muddy the number.
    """
    sql = _FakeSQL()
    store = PostgresDeliveryDedupeStore(sql, ttl_seconds=_VIRTUAL_TTL)
    for n in range(3):
        assert await store.claim("github", f"d{n}") is True
    sql.advance(_VIRTUAL_TTL * 10)
    assert await store.claim("github", "recent") is True

    assert await store.purge(older_than_seconds=_VIRTUAL_TTL * 5) == 3
    assert set(sql.rows) == {("github", "recent")}


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
