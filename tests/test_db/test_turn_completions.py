"""Contract suite for the completion ledger.

Runs against the PostgreSQL store (through a SQL-executing fake), the
memory twin, and — when ``CREWLET_TEST_DSN`` is set — a real database.

What this table is NOT is half the point, so the tests say it: there is
no ``in_progress`` state, no expiry, and no supersede rule. A row means
the work is done, and done does not lapse. The design this replaced had
all three, and five of five reviewers returned ``redesign-needed`` on it
because ``in_progress`` bought no exclusion the seat lease did not
already provide while generating every other defect they found.
"""

from __future__ import annotations

import os
from typing import Any

import pytest

from crewlet.db.turn_completions import (
    MemoryTurnCompletionStore,
    PostgresTurnCompletionStore,
)

_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")


class _FakeSQL:
    """Applies the store's SQL semantically, including the
    first-writer-wins conflict clause."""

    def __init__(self) -> None:
        self.rows: dict[tuple[str, str], dict[str, Any]] = {}
        self.fail = False

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        if self.fail:
            raise RuntimeError("database is down")
        if query.lstrip().startswith("SELECT"):
            seat, ids = str(args[0]), list(args[1])
            return [{"trigger_id": tid} for tid in ids if (seat, tid) in self.rows]
        if query.lstrip().startswith("INSERT"):
            seat, ids = str(args[0]), list(args[1])
            written = []
            for tid in ids:
                if (seat, tid) in self.rows:
                    continue
                self.rows[(seat, tid)] = {
                    "turn_id": args[2],
                    "node": args[3],
                    "owner_epoch": args[4],
                }
                written.append({"trigger_id": tid})
            return written
        # DELETE … the fake has no clock, so retention is exercised
        # against the real database rather than pretended at here.
        return []


class _RealSQL:
    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._db: Any = None

    async def _pool(self) -> Any:
        if self._db is None:
            from crewlet.db.client import Database

            self._db = await Database.connect(self._dsn)
            await self._db.execute("DELETE FROM turn_completions")
        return self._db

    async def aclose(self) -> None:
        if self._db is not None:
            await self._db.close()
            self._db = None

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        return await (await self._pool()).execute(query, *args)

    async def age_async(self, seconds: float) -> None:
        await (await self._pool()).execute(
            "UPDATE turn_completions "
            "SET completed_at = completed_at - make_interval(secs => $1)",
            float(seconds),
        )


def _memory() -> Any:
    return MemoryTurnCompletionStore()


def _postgres() -> Any:
    return PostgresTurnCompletionStore(_FakeSQL())


def _real_postgres() -> Any:
    return PostgresTurnCompletionStore(_RealSQL(_TEST_DSN))


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
    inner = getattr(built, "_db", None)
    if isinstance(inner, _RealSQL):
        await inner.aclose()


# ── recording and reading ────────────────────────────────────────────


async def test_an_unrecorded_trigger_is_not_completed(store: Any) -> None:
    """The default answer, and the one every failure degrades to."""
    assert await store.completed("eng", ["t1", "t2"]) == set()


async def test_a_recorded_trigger_reads_back(store: Any) -> None:
    assert await store.record("eng", ["t1"]) == 1
    assert await store.completed("eng", ["t1", "t2"]) == {"t1"}


async def test_seats_are_namespaced(store: Any) -> None:
    """Two seats can be handed the same trigger — a task assigned to a
    lead and its report — and neither answers for the other."""
    await store.record("eng", ["shared"])
    assert await store.completed("ops", ["shared"]) == set()


async def test_recording_is_first_writer_wins(store: Any) -> None:
    """A zombie recording a turn its successor already recorded is a
    no-op. Either way the trigger ends up marked done exactly once."""
    assert await store.record("eng", ["t1"], node="node-a:1", owner_epoch=4) == 1
    assert await store.record("eng", ["t1"], node="node-b:9", owner_epoch=5) == 0
    assert await store.completed("eng", ["t1"]) == {"t1"}


async def test_a_partial_overlap_records_only_the_new_ones(store: Any) -> None:
    """The case a single-key design could not express at all.

    A redelivered partition can overlap a previous one partially — A+B
    ran, then A+B+C arrives because C landed in the gap. Only C is new.
    """
    await store.record("eng", ["a", "b"])
    assert await store.record("eng", ["a", "b", "c"]) == 1
    assert await store.completed("eng", ["a", "b", "c"]) == {"a", "b", "c"}


async def test_empty_inputs_are_no_ops(store: Any) -> None:
    assert await store.record("eng", []) == 0
    assert await store.record("", ["t1"]) == 0
    assert await store.completed("eng", []) == set()
    assert await store.completed("", ["t1"]) == set()


# ── the failure policy IS the design ─────────────────────────────────


async def test_an_unreadable_store_reports_nothing_completed() -> None:
    """Fails OPEN, deliberately.

    "I cannot tell whether this was done" has exactly one safe answer,
    and it is the answer the engine gave before this table existed: run
    it. Failing closed would park real work during a database blip — and
    the seat's own admission gate already refuses new turns within one
    heartbeat of a store it cannot reach.
    """
    sql = _FakeSQL()
    store = PostgresTurnCompletionStore(sql)
    await store.record("eng", ["t1"])
    sql.fail = True
    assert await store.completed("eng", ["t1"]) == set()


async def test_an_unwritable_store_does_not_raise() -> None:
    """Also open, for a different reason: by the time this is written
    the turn's side effects have already shipped, and refusing to record
    them cannot un-ship them. The cost is at most one duplicate turn —
    which is exactly the behaviour without a ledger."""
    sql = _FakeSQL()
    sql.fail = True
    store = PostgresTurnCompletionStore(sql)
    assert await store.record("eng", ["t1"]) == 0


# ── retention ────────────────────────────────────────────────────────


async def test_purge_drops_old_rows_and_keeps_recent_ones() -> None:
    store = MemoryTurnCompletionStore()
    await store.record("eng", ["old"])
    assert await store.purge(older_than_seconds=-1) == 1
    assert await store.completed("eng", ["old"]) == set()

    await store.record("eng", ["fresh"])
    assert await store.purge(older_than_seconds=3600) == 0
    assert await store.completed("eng", ["fresh"]) == {"fresh"}


async def test_retention_outlives_the_brokers_redelivery_window() -> None:
    """The floor is how long a trigger can keep coming back.

    The ack timeout times the redelivery budget is about five hours, and
    an unowned seat holds its backlog for as long as it stays unowned —
    which has no bound at all. A retention shorter than that would stop
    the mechanism working exactly when it is needed, since a long
    unowned window is precisely when a redelivery is old.
    """
    from crewlet.db.turn_completions import TURN_COMPLETION_RETENTION_SECONDS
    from crewlet.queue.pulsar import _INBOX_ACK_TIMEOUT_MS, _MAX_REDELIVER

    redelivery_window = (_INBOX_ACK_TIMEOUT_MS / 1000.0) * _MAX_REDELIVER
    assert redelivery_window < TURN_COMPLETION_RETENTION_SECONDS
