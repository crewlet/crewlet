"""Contract suite for fleet-wide credential cooldowns."""

from __future__ import annotations

from typing import Any

import pytest

from crewlet.db.credentials import (
    CooldownSync,
    MemoryCredentialCooldownStore,
    PostgresCredentialCooldownStore,
)
from crewlet.providers.credential import (
    CooldownPolicy,
    CredentialEntry,
    CredentialPool,
    short_hash,
)


class _FakeSQL:
    """Applies the store's SQL semantically, including the GREATEST
    upsert (a longer cooldown must never be shortened)."""

    def __init__(self) -> None:
        self.rows: dict[tuple[str, str], float] = {}
        self.now = 1000.0
        self.fail = False

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        if self.fail:
            raise RuntimeError("database is down")
        q = " ".join(query.split())
        if q.startswith("INSERT INTO credential_cooldowns"):
            key = (args[0], args[1])
            until = self.now + float(args[2])
            self.rows[key] = max(self.rows.get(key, 0.0), until)
            return []
        if q.startswith("SELECT credential_hint"):
            return [
                {"credential_hint": hint, "remaining": until - self.now}
                for (provider, hint), until in self.rows.items()
                if provider == args[0] and until > self.now
            ]
        raise AssertionError(f"unexpected execute: {q}")


def _memory() -> Any:
    return MemoryCredentialCooldownStore()


def _postgres() -> Any:
    return PostgresCredentialCooldownStore(_FakeSQL())


@pytest.fixture(
    params=[pytest.param(_memory, id="memory"), pytest.param(_postgres, id="postgres")]
)
def store(request: pytest.FixtureRequest) -> Any:
    return request.param()


async def test_cooling_a_key_is_visible_to_a_peer(store: Any) -> None:
    """The point: only the replica that saw the 429 used to know."""
    await store.cool("default", "hint-a", seconds=60, reason="rate_limit")
    active = await store.active("default")
    assert "hint-a" in active
    assert active["hint-a"] > 0


async def test_providers_are_namespaced(store: Any) -> None:
    await store.cool("anthropic", "shared-hint", seconds=60)
    assert "shared-hint" not in await store.active("openai")


async def test_a_longer_cooldown_never_shortens(store: Any) -> None:
    await store.cool("default", "hint-a", seconds=600)
    await store.cool("default", "hint-a", seconds=5)
    assert (await store.active("default"))["hint-a"] > 100


async def test_blank_hint_and_zero_seconds_are_ignored(store: Any) -> None:
    await store.cool("default", "", seconds=60)
    await store.cool("default", "hint-a", seconds=0)
    assert await store.active("default") == {}


async def test_write_failure_does_not_raise() -> None:
    """Cooldown bookkeeping must never fail an LLM call — the local
    cooldown still applies, the fleet just learns late."""
    sql = _FakeSQL()
    store = PostgresCredentialCooldownStore(sql)
    sql.fail = True
    await store.cool("default", "hint-a", seconds=60)
    assert await store.active("default") == {}


# --- the pool -------------------------------------------------------------


def _pool(**kwargs: Any) -> CredentialPool:
    return CredentialPool(
        # Production always sets a hint (see the openai/anthropic
        # providers); it is the non-secret name a cooldown is shared
        # under, so a hintless entry silently shares nothing.
        entries=[CredentialEntry(value=v, hint=short_hash(v)) for v in ("k1", "k2")],
        cooldowns=CooldownPolicy(rate_limit_seconds=60),
        **kwargs,
    )


async def test_pool_shares_a_cooldown_it_records() -> None:
    shared = _memory()
    pool = _pool(cooldown_sync=CooldownSync(shared, "default"))

    entry = await pool.acquire()
    await pool.mark_cooled(entry, seconds=60)

    assert entry.hint in await shared.active("default")


async def test_pool_adopts_a_peers_cooldown() -> None:
    """Without this, a peer keeps hammering a rate-limited key for the
    full cooldown — and once every key looks cooled locally, the fallback
    chain moves the seat to a different model."""
    shared = _memory()
    node_a = _pool(cooldown_sync=CooldownSync(shared, "default"))
    node_b = _pool(cooldown_sync=CooldownSync(shared, "default", refresh_seconds=0))

    entry = await node_a.acquire()
    await node_a.mark_cooled(entry, seconds=60)

    # node_b's own entries were never cooled locally.
    await node_b._sync_peer_cooldowns(force=True)
    cooled = [e for e in node_b.entries if e.hint == entry.hint]
    assert cooled and cooled[0].cooldown_until > 0


async def test_pool_without_a_sync_stays_process_local() -> None:
    pool = _pool()
    entry = await pool.acquire()
    await pool.mark_cooled(entry, seconds=60)  # must not raise
    assert entry.cooldown_until > 0
