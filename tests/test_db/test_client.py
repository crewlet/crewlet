"""Connection-holding semantics of :class:`crewlet.db.client.Database`.

The advisory lock is taken on the boot path of every process, so what
it does when the connection under it goes bad decides what an operator
sees when a migration fails.
"""

from __future__ import annotations

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Any

import pytest

from crewlet.db.client import Database


class _FakeDB(Database):
    """A ``Database`` whose one connection can be made to misbehave.

    Subclasses the real class so ``advisory_lock`` runs verbatim; only
    the statements it issues are answered by hand.
    """

    def __init__(self, *, unlock: Any = True) -> None:
        super().__init__()
        self._unlock = unlock
        self.unlock_attempts = 0

    async def fetchval(self, sql: str, *args: Any) -> Any:
        if "pg_try_advisory_lock" in sql:
            return True
        if "pg_advisory_unlock" in sql:
            self.unlock_attempts += 1
            if isinstance(self._unlock, Exception):
                raise self._unlock
            return self._unlock
        return None

    @asynccontextmanager
    async def acquire(self) -> AsyncIterator[Any]:
        yield self


async def test_a_failed_unlock_does_not_mask_the_blocks_own_error() -> None:
    """The migrator's real failure must survive the cleanup.

    A connection that drops mid-migration fails the statement AND the
    unlock that follows it. Raising from the ``finally`` replaces the
    error that says which migration broke with a bare connection error
    from the lock helper — the operator is told the wrong thing about
    the wrong line.
    """
    db = _FakeDB(unlock=ConnectionResetError("connection is closed"))

    with pytest.raises(RuntimeError, match="migration 011 failed"):
        async with db.advisory_lock(4242):
            raise RuntimeError("migration 011 failed: syntax error at or near")

    assert db.unlock_attempts == 1, "the unlock must still be attempted"


async def test_a_failed_unlock_does_not_break_a_clean_block() -> None:
    db = _FakeDB(unlock=ConnectionResetError("connection is closed"))

    async with db.advisory_lock(4242):
        pass

    assert db.unlock_attempts == 1


async def test_an_unlock_that_reports_not_held_is_announced(caplog) -> None:
    """``pg_advisory_unlock`` returning false means the session was reset
    underneath us — the lock is not ours to release and, worse, was not
    ours to hold. Silence here means the next waiter's five-minute
    timeout is the first anyone hears of it."""
    db = _FakeDB(unlock=False)

    with caplog.at_level("WARNING"):
        async with db.advisory_lock(4242):
            pass

    assert "advisory_lock_not_held_at_release" in caplog.text
