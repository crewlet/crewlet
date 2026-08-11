"""Credential pool internals for the multi-key rotation feature.

A provider holds a small pool of API keys. Each call leases the next
available key; if the call returns a rate-limit / auth error, the pool
marks the key cooled-down for a TTL and the next call picks a
different key. When every key is in cooldown, the pool raises
:class:`~crewlet.providers.errors.AllCredentialsExhausted`, which the
provider fallback wrapper catches.

Crewlet runs many agents concurrently in one process, and provider
instances are *shared* across all of them -- so the pool is a
contended resource. Selection + cooldown bookkeeping run under an
``asyncio.Lock``, and :meth:`CredentialPool.acquire` spreads
concurrent leases across keys by picking the live key with the fewest
in-flight calls (declaration order breaks ties, so a single-caller
workload still behaves fill-first). Callers must :meth:`release` the
entry when the call completes -- a ``try/finally`` around the SDK
call.

Cooldowns are measured on ``time.monotonic()`` (via
:func:`now_seconds`) so a wall-clock jump (NTP correction, VM
migration, container clock skew) cannot revive a cooled key early or
strand a live one.
"""

from __future__ import annotations

import asyncio
import hashlib
import time
from dataclasses import dataclass, field

# Cap on the exponential-backoff multiplier for repeated AUTH failures
# on one key. A genuinely-bad key (typo'd credential) would otherwise
# thrash -- 401, cool for auth_seconds, retry, 401 again, forever. Each
# consecutive AUTH failure doubles the cooldown up to this many
# doublings; a single success resets the counter.
_MAX_AUTH_BACKOFF_DOUBLINGS = 6


@dataclass
class CredentialEntry:
    """One key in a provider's pool."""

    value: str
    cooldown_until: float = 0.0
    """``time.monotonic()`` timestamp. ``0`` means "ready". Set by
    :meth:`CredentialPool.mark_cooled` on a rate-limit / auth error."""
    use_count: int = 0
    """Cumulative number of leases that returned this key. Used by
    operators reading the pool's status for hot-spotting."""
    in_flight: int = 0
    """Leases currently out (acquired, not yet released). Drives
    least-loaded selection so concurrent callers spread across keys
    instead of all hammering the declaration-order head."""
    consecutive_auth_failures: int = 0
    """Count of AUTH failures since the last success on this key.
    Drives exponential backoff so a permanently-bad key stops
    thrashing; reset to 0 by :meth:`CredentialPool.release` with
    ``succeeded=True``."""
    hint: str = ""
    """Stable, non-secret identifier for logs / events. Computed once
    via :func:`short_hash` -- 12 hex characters of SHA-256, ample
    entropy to disambiguate keys without leaking the value or
    enabling a substring attack against short keys."""


def short_hash(value: str) -> str:
    """Return a 12-char SHA-256-prefix hint for a credential value.

    Used as the ``CredentialEntry.hint`` and in event payloads
    (``CredentialExhausted.credential_hint``). Stable across runs so
    operators can correlate cooldown events to specific keys without
    the key itself appearing in any log or event.

    12 hex characters = 48 bits of entropy: enough to disambiguate
    realistic pool sizes (a handful of keys) while remaining
    impossible to reverse.
    """
    if not value:
        return ""
    return hashlib.sha256(value.encode("utf-8")).hexdigest()[:12]


def now_seconds() -> float:
    """Monotonic "now" for cooldown arithmetic.

    Wraps ``time.monotonic()`` -- immune to wall-clock jumps -- and is
    a single seam tests monkeypatch to fast-forward cooldown TTLs
    without sleeping.
    """
    return time.monotonic()


@dataclass
class CooldownPolicy:
    """TTL policy for credential cooldowns. Each error class gets its
    own duration -- 429s usually clear in an hour while 401s often
    clear in minutes (token refresh)."""

    rate_limit_seconds: int = 3600
    auth_seconds: int = 300


@dataclass
class CredentialPool:
    """A small bag of credentials with cooldown bookkeeping.

    The pool is owned by each provider instance and shared across every
    agent turn that provider backs, so all mutating operations run
    under :attr:`_lock`. The provider's ``complete`` method calls
    :meth:`acquire`, makes the SDK call, calls :meth:`mark_cooled` on a
    rate-limit / auth error, and :meth:`release` in a ``finally``. When
    every key is cooled the provider raises
    :class:`AllCredentialsExhausted` -- handled by the multi-provider fallback
    wrapper.
    """

    entries: list[CredentialEntry]
    cooldowns: CooldownPolicy = field(default_factory=CooldownPolicy)
    _lock: asyncio.Lock = field(
        default_factory=asyncio.Lock, init=False, repr=False, compare=False
    )

    async def acquire(self) -> CredentialEntry | None:
        """Return a ready credential or ``None`` when every key is in
        cooldown.

        Among the keys not in cooldown, picks the one with the fewest
        in-flight leases so concurrent callers (the common case in
        Crewlet -- one shared provider, many agents) actually spread
        across the pool instead of all leasing the declaration-order
        head. ``min`` keeps declaration order as the tiebreaker, so a
        single-caller workload still behaves fill-first.

        Increments ``use_count`` and ``in_flight``; the caller must
        :meth:`release` when the call completes.
        """
        async with self._lock:
            wall = now_seconds()
            live = [e for e in self.entries if e.cooldown_until <= wall]
            if not live:
                return None
            entry = min(live, key=lambda e: e.in_flight)
            entry.use_count += 1
            entry.in_flight += 1
            return entry

    async def release(self, entry: CredentialEntry, *, succeeded: bool) -> None:
        """Return a leased ``entry`` to the pool.

        ``succeeded=True`` (the SDK call returned a completion) also
        resets the entry's consecutive-AUTH-failure counter -- a key
        that just worked is not in a bad-credential backoff.
        """
        async with self._lock:
            if entry.in_flight > 0:
                entry.in_flight -= 1
            if succeeded:
                entry.consecutive_auth_failures = 0

    async def mark_cooled(
        self,
        entry: CredentialEntry,
        *,
        seconds: int,
        is_auth: bool = False,
    ) -> None:
        """Put ``entry`` in cooldown for ``seconds`` starting now.

        When ``is_auth`` is set the entry's consecutive-AUTH-failure
        counter is incremented and the cooldown is multiplied by
        ``2 ** (failures - 1)`` (capped at
        ``_MAX_AUTH_BACKOFF_DOUBLINGS`` doublings). A permanently-bad
        key therefore backs off exponentially instead of thrashing the
        provider every ``auth_seconds``; one successful call (via
        :meth:`release`) resets the counter.
        """
        async with self._lock:
            base = max(0, int(seconds))
            if is_auth:
                entry.consecutive_auth_failures += 1
                doublings = min(
                    entry.consecutive_auth_failures - 1,
                    _MAX_AUTH_BACKOFF_DOUBLINGS,
                )
                base *= 2**doublings
            entry.cooldown_until = now_seconds() + base


__all__ = [
    "CooldownPolicy",
    "CredentialEntry",
    "CredentialPool",
    "short_hash",
]
