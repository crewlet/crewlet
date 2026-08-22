"""Contract suite for the per-conversation session ledger.

Runs against the PostgreSQL store (through a SQL-executing fake), the
memory twin, and — when ``CREWLET_TEST_DSN`` is set — a real database.

The two properties that are easy to get wrong and expensive to get
wrong are asserted directly: appends dedupe on the WORK key rather than
the turn id (two nodes completing one trigger mint two turn ids, so a
turn-keyed row records the duplicate instead of collapsing it), and the
stored depth is bounded at write time (a chat DM keys on the whole
channel, so its ledger never stops receiving entries).
"""

from __future__ import annotations

import os
from typing import Any

import pytest

from crewlet.db.conversation_sessions import (
    MemoryConversationSessionStore,
    PostgresConversationSessionStore,
)

_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")


class _FakeSQL:
    """Applies the store's SQL semantically, conflict clause included."""

    def __init__(self) -> None:
        # (agent, conversation, work_key) -> row, insertion-ordered so
        # "newest last" holds without a clock.
        self.rows: dict[tuple[str, str, str], dict[str, Any]] = {}
        self.fail = False

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        if self.fail:
            raise RuntimeError("database is down")
        head = query.lstrip().split()[0].upper()
        if head == "INSERT":
            agent, conversation, work_key, turn_id, entry = args[:5]
            key = (agent, conversation, work_key)
            if key in self.rows:
                return []  # ON CONFLICT DO NOTHING
            self.rows[key] = {
                "turn_id": turn_id,
                "work_key": work_key,
                "created_at": None,
                "entry": entry,
            }
            return [{"work_key": work_key}]
        if head == "DELETE" and "OFFSET" in query:
            agent, conversation, keep = args[0], args[1], int(args[2])
            mine = [k for k in self.rows if k[0] == agent and k[1] == conversation]
            for key in mine[: max(0, len(mine) - keep)]:
                del self.rows[key]
            return []
        if head == "SELECT" and "GROUP BY" in query:
            agent = args[0]
            seen: dict[str, int] = {}
            for a, conversation, _wk in self.rows:
                if a == agent:
                    seen[conversation] = seen.get(conversation, 0) + 1
            return [
                {"conversation_key": c, "entries": n, "last_at": None}
                for c, n in seen.items()
            ]
        if head == "SELECT":
            agent, conversation, limit = args[0], args[1], int(args[2])
            mine = [
                dict(v)
                for k, v in self.rows.items()
                if k[0] == agent and k[1] == conversation
            ]
            # The store's SQL is ORDER BY created_at DESC LIMIT n; the
            # fake has no clock, so insertion order stands in for it.
            return list(reversed(mine))[:limit]
        # DELETE … WHERE created_at < … — retention needs a clock, so it
        # is exercised against the real database rather than pretended
        # at here.
        return []


class _RealSQL:
    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._db: Any = None

    async def _pool(self) -> Any:
        if self._db is None:
            from crewlet.db.client import Database

            self._db = await Database.connect(self._dsn)
            await self._db.execute("DELETE FROM conversation_sessions")
        return self._db

    async def aclose(self) -> None:
        if self._db is not None:
            await self._db.close()
            self._db = None

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        return await (await self._pool()).execute(query, *args)

    async def age_async(self, seconds: float) -> None:
        await (await self._pool()).execute(
            "UPDATE conversation_sessions "
            "SET created_at = created_at - make_interval(secs => $1)",
            float(seconds),
        )


def _memory() -> Any:
    return MemoryConversationSessionStore()


def _postgres() -> Any:
    return PostgresConversationSessionStore(_FakeSQL())


def _real_postgres() -> Any:
    return PostgresConversationSessionStore(_RealSQL(_TEST_DSN))


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


async def _append(store: Any, **kw: Any) -> bool:
    base = {
        "agent_handle": "eng",
        "conversation_key": "jira:POC-7",
        "work_key": "w1",
        "turn_id": "t1",
        "entry": {"reply": "on it"},
    }
    base.update(kw)
    return await store.append(**base)


# ── appending and reading back ───────────────────────────────────────


async def test_a_fresh_conversation_has_no_history(store: Any) -> None:
    """The first turn of every conversation, and the answer every
    failure degrades to."""
    rows = await store.recent(
        agent_handle="eng", conversation_key="jira:POC-7", limit=5
    )
    assert rows == []


async def test_an_appended_entry_reads_back(store: Any) -> None:
    assert await _append(store) is True
    rows = await store.recent(
        agent_handle="eng", conversation_key="jira:POC-7", limit=5
    )
    assert len(rows) == 1
    assert rows[0].entry == {"reply": "on it"}
    assert rows[0].work_key == "w1"


async def test_conversations_are_namespaced(store: Any) -> None:
    """Two threads the same seat works are separate histories — the
    whole point of keying on the conversation."""
    await _append(store, conversation_key="jira:POC-7", work_key="w1")
    await _append(store, conversation_key="slack:C1:99", work_key="w2")

    rows = await store.recent(
        agent_handle="eng", conversation_key="jira:POC-7", limit=5
    )
    assert [r.work_key for r in rows] == ["w1"]


async def test_seats_are_namespaced(store: Any) -> None:
    """Two seats legitimately serve one conversation — a lead and its
    report on the same ticket — and each one's ledger is its own."""
    await _append(store, agent_handle="eng", work_key="w1")

    rows = await store.recent(
        agent_handle="ops", conversation_key="jira:POC-7", limit=5
    )
    assert rows == []


async def test_entries_read_back_oldest_first(store: Any) -> None:
    """Rendering order. The block reads as a conversation, so the
    turn being answered is last, nearest the ask."""
    for i in range(3):
        await _append(store, work_key=f"w{i}", entry={"reply": str(i)})

    rows = await store.recent(
        agent_handle="eng", conversation_key="jira:POC-7", limit=5
    )
    assert [r.entry["reply"] for r in rows] == ["0", "1", "2"]


async def test_recent_returns_the_NEWEST_when_limited(store: Any) -> None:
    """Under a limit it is the most recent turns that matter — the one
    most likely to have already answered what just came in."""
    for i in range(5):
        await _append(store, work_key=f"w{i}", entry={"reply": str(i)})

    rows = await store.recent(
        agent_handle="eng", conversation_key="jira:POC-7", limit=2
    )
    assert [r.entry["reply"] for r in rows] == ["3", "4"]


# ── dedupe: the work key, never the turn id ──────────────────────────


async def test_appending_is_first_writer_wins_on_the_work_key(store: Any) -> None:
    """Two nodes completing one trigger mint two turn ids.

    Keyed on the turn id, this table would RECORD that duplicate instead
    of collapsing it, and the conversation's next turn would read its
    own reply twice and conclude it had said the same thing twice.
    """
    assert await _append(store, work_key="w1", turn_id="turn-a") is True
    assert await _append(store, work_key="w1", turn_id="turn-b") is False

    rows = await store.recent(
        agent_handle="eng", conversation_key="jira:POC-7", limit=5
    )
    assert len(rows) == 1
    assert rows[0].turn_id == "turn-a"


async def test_the_same_work_key_in_another_conversation_still_lands(
    store: Any,
) -> None:
    """Dedupe is per conversation, not global."""
    assert await _append(store, conversation_key="jira:POC-7", work_key="w1") is True
    assert await _append(store, conversation_key="slack:C1:99", work_key="w1") is True


# ── bounded at write time ────────────────────────────────────────────


async def test_the_ledger_is_trimmed_to_max_entries(store: Any) -> None:
    """A chat DM keys on the whole channel rather than a thread, so its
    ledger never stops receiving entries. The trim is what bounds it."""
    for i in range(6):
        await _append(store, work_key=f"w{i}", entry={"reply": str(i)}, max_entries=3)

    rows = await store.recent(
        agent_handle="eng", conversation_key="jira:POC-7", limit=50
    )
    assert [r.entry["reply"] for r in rows] == ["3", "4", "5"]


# ── incomplete identity is not recorded ──────────────────────────────


@pytest.mark.parametrize("missing", ["agent_handle", "conversation_key", "work_key"])
async def test_an_entry_with_no_identity_is_refused(store: Any, missing: str) -> None:
    """Each of the three is part of the row's identity; without one
    there is nothing to key on and nothing that could read it back."""
    assert await _append(store, **{missing: ""}) is False


async def test_a_zero_limit_reads_nothing(store: Any) -> None:
    await _append(store)
    assert (
        await store.recent(agent_handle="eng", conversation_key="jira:POC-7", limit=0)
        == []
    )


# ── failing open ─────────────────────────────────────────────────────


async def test_an_unreadable_store_raises_rather_than_reading_empty() -> None:
    """ "Cannot read" and "nothing said yet" must not be one answer.

    Each caller decides what to do about it — the turn engine renders no
    history (the pre-ledger prompt), the API reports that it could not
    see the ledger. Swallowing it here would take that choice away and
    make an operator screen draw a database outage as a silent seat.
    """
    sql = _FakeSQL()
    store = PostgresConversationSessionStore(sql)
    await store.append(
        agent_handle="eng",
        conversation_key="jira:POC-7",
        work_key="w1",
        turn_id="t1",
        entry={},
    )
    sql.fail = True

    with pytest.raises(RuntimeError):
        await store.recent(agent_handle="eng", conversation_key="jira:POC-7", limit=5)
    with pytest.raises(RuntimeError):
        await store.conversations(agent_handle="eng")


async def test_an_unwritable_store_does_not_raise() -> None:
    """The turn is complete and its side effects shipped; refusing to
    record them cannot un-ship them, and must not fail the turn."""
    sql = _FakeSQL()
    sql.fail = True
    store = PostgresConversationSessionStore(sql)

    assert (
        await store.append(
            agent_handle="eng",
            conversation_key="jira:POC-7",
            work_key="w1",
            turn_id="t1",
            entry={},
        )
        is False
    )


# ── listing a seat's conversations ───────────────────────────────────


async def test_conversations_lists_what_a_seat_has_worked(store: Any) -> None:
    await _append(store, conversation_key="jira:POC-7", work_key="w1")
    await _append(store, conversation_key="jira:POC-7", work_key="w2")
    await _append(store, conversation_key="slack:C1:99", work_key="w3")

    listed = await store.conversations(agent_handle="eng")

    by_key = {c["conversation_key"]: c["entries"] for c in listed}
    assert by_key == {"jira:POC-7": 2, "slack:C1:99": 1}


async def test_conversations_is_scoped_to_the_seat(store: Any) -> None:
    await _append(store, agent_handle="eng", work_key="w1")
    assert await store.conversations(agent_handle="ops") == []


# ── retention (real database only — the fake has no clock) ───────────


@pytest.mark.integration
@pytest.mark.skipif(not _TEST_DSN, reason="set CREWLET_TEST_DSN to run against real PG")
async def test_purge_drops_rows_past_the_horizon() -> None:
    sql = _RealSQL(_TEST_DSN)
    store = PostgresConversationSessionStore(sql)
    try:
        await store.append(
            agent_handle="eng",
            conversation_key="jira:POC-7",
            work_key="w1",
            turn_id="t1",
            entry={},
        )
        await sql.age_async(40 * 24 * 3600.0)

        assert await store.purge(30 * 24 * 3600.0) == 1
        assert (
            await store.recent(
                agent_handle="eng", conversation_key="jira:POC-7", limit=5
            )
            == []
        )
    finally:
        await sql.aclose()
