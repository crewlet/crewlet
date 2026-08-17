"""Fleet-wide LLM credential cooldowns.

A 429 cools a key so the next call rotates to another. Kept in the
process that saw the error, that knowledge never reaches the peers still
hammering the same key — so the org earns N times the rate-limit errors
and the rotation feature is defeated in exact proportion to replica
count. Worse, once every key in a pool looks cooled the provider raises
``AllCredentialsExhausted`` and the fallback chain moves to the next
provider, so two replicas can answer the same seat on different models.

Cooldowns are shared as wall-clock deadlines keyed by the pool's existing
credential *hint* — a sha256 prefix, never the key — so nothing secret
lands in the table.
"""

from __future__ import annotations

import time
from datetime import UTC, datetime, timedelta
from typing import Any, Protocol

from crewlet._logging import get_logger

logger = get_logger("db.credentials")

# How often a pool re-reads peer cooldowns.
#
# Not on the hot path: `acquire` uses its local view and refreshes at
# most this often, so a steady stream of LLM calls costs one small query
# per interval rather than one per call. 30s bounds how long a peer can
# keep hammering a key another replica already cooled, against a
# rate-limit cooldown measured in minutes-to-an-hour — small enough to
# matter, large enough to be free.
COOLDOWN_REFRESH_SECONDS = 30.0


class CredentialCooldownStore(Protocol):
    """Shared record of which credentials are cooling, and until when."""

    async def cool(
        self, provider_key: str, hint: str, *, seconds: float, reason: str = ""
    ) -> None: ...

    async def active(self, provider_key: str) -> dict[str, float]:
        """``{hint: seconds_remaining}`` for cooldowns still in force."""
        ...


class PostgresCredentialCooldownStore:
    """PostgreSQL-backed :class:`CredentialCooldownStore`."""

    def __init__(self, db: Any) -> None:
        self._db = db

    async def cool(
        self, provider_key: str, hint: str, *, seconds: float, reason: str = ""
    ) -> None:
        if not hint or seconds <= 0:
            return
        try:
            await self._db.execute(
                """
                INSERT INTO credential_cooldowns
                    (provider_key, credential_hint, cooled_until, reason)
                VALUES ($1, $2, now() + make_interval(secs => $3), $4)
                ON CONFLICT (provider_key, credential_hint) DO UPDATE
                SET cooled_until = GREATEST(
                        credential_cooldowns.cooled_until, EXCLUDED.cooled_until
                    ),
                    reason     = EXCLUDED.reason,
                    updated_at = now()
                """,
                provider_key,
                hint,
                float(seconds),
                reason,
            )
        except Exception:
            # Never fail an LLM call over cooldown bookkeeping — the
            # local cooldown still applies, the fleet just learns late.
            logger.warning("credential_cooldown_write_failed", provider=provider_key)

    async def active(self, provider_key: str) -> dict[str, float]:
        try:
            rows = await self._db.execute(
                """
                SELECT credential_hint,
                       EXTRACT(EPOCH FROM (cooled_until - now())) AS remaining
                FROM credential_cooldowns
                WHERE provider_key = $1 AND cooled_until > now()
                """,
                provider_key,
            )
        except Exception:
            logger.warning("credential_cooldown_read_failed", provider=provider_key)
            return {}
        return {
            str(r["credential_hint"]): max(0.0, float(r["remaining"] or 0.0))
            for r in rows
        }


class MemoryCredentialCooldownStore:
    """In-memory :class:`CredentialCooldownStore` twin."""

    def __init__(self) -> None:
        self._until: dict[tuple[str, str], datetime] = {}

    async def cool(
        self, provider_key: str, hint: str, *, seconds: float, reason: str = ""
    ) -> None:
        if not hint or seconds <= 0:
            return
        deadline = datetime.now(UTC) + timedelta(seconds=seconds)
        key = (provider_key, hint)
        # GREATEST semantics: a longer cooldown never shortens.
        existing = self._until.get(key)
        self._until[key] = max(existing, deadline) if existing else deadline

    async def active(self, provider_key: str) -> dict[str, float]:
        now = datetime.now(UTC)
        return {
            hint: (deadline - now).total_seconds()
            for (provider, hint), deadline in self._until.items()
            if provider == provider_key and deadline > now
        }


class CooldownSync:
    """Throttled read-through of peer cooldowns for one credential pool.

    Owns the "have I looked recently" bookkeeping so the pool's hot path
    stays a local comparison.
    """

    def __init__(
        self,
        store: CredentialCooldownStore,
        provider_key: str,
        *,
        refresh_seconds: float = COOLDOWN_REFRESH_SECONDS,
        now: Any = time.monotonic,
    ) -> None:
        self._store = store
        self._provider_key = provider_key
        self._refresh = refresh_seconds
        self._now = now
        self._last = float("-inf")

    async def cool(self, hint: str, seconds: float, reason: str = "") -> None:
        await self._store.cool(self._provider_key, hint, seconds=seconds, reason=reason)

    async def peer_cooldowns(self, *, force: bool = False) -> dict[str, float]:
        """Peer cooldowns, at most once per refresh interval.

        ``force`` bypasses the throttle — used when every local key looks
        cooled, because that is the moment the answer changes what
        happens (fall through to another provider, or wait).
        """
        now = self._now()
        if not force and now - self._last < self._refresh:
            return {}
        self._last = now
        return await self._store.active(self._provider_key)
