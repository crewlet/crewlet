"""Contract suite for durable A2A channel state.

This used to be a dict on the service, and every authorization decision
reads it: may this sender post, may this closer close, who is the other
party. On one node a dict answers correctly. On two it answers *"channel
is not open or does not exist"* for a channel that is open — because the
target of an ask wakes on the node that owns its seat, which is rarely
the node that opened the channel.
"""

from __future__ import annotations

import os
from datetime import UTC, datetime, timedelta
from typing import Any

import pytest

from crewlet.a2a.channels import (
    MemoryA2AChannelStore,
    PostgresA2AChannelStore,
)

_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")


class _FakeSQL:
    """Applies the store's SQL semantically.

    It carries ``opened_at`` / ``closed_at`` and honours the idle
    sweep's interval, because both used to be missing and both hid
    something. Rows with no timestamps made ``_row_to_channel``\'s
    contract untestable here; and an idle sweep that ignored
    ``older_than_seconds`` closed every open channel whatever was
    asked, so a statement that dropped the interval entirely would
    still have passed on this backend.
    """

    def __init__(self) -> None:
        self.rows: dict[str, dict[str, Any]] = {}

    def _now(self) -> datetime:
        return datetime.now(UTC)

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        head = query.strip().split()[0].upper()
        if head == "INSERT":
            self.rows.setdefault(
                str(args[0]),
                {
                    "channel_id": args[0],
                    "requester": args[1],
                    "target": args[2],
                    "state": "open",
                    "message_count": 0,
                    "opened_at": self._now(),
                    "closed_at": None,
                },
            )
            return []
        if head == "UPDATE" and "state = 'closed'" in query and "opened_at <" in query:
            cutoff = self._now() - timedelta(seconds=float(args[0]))
            closed = [
                r
                for r in self.rows.values()
                if r["state"] == "open" and r["opened_at"] < cutoff
            ]
            for row in closed:
                row["state"] = "closed"
                row["closed_at"] = self._now()
            return [dict(r) for r in closed]
        if head == "UPDATE" and "state = 'closed'" in query:
            row = self.rows.get(str(args[0]))
            if row is None or row["state"] != "open":
                return []
            row["state"] = "closed"
            row["closed_at"] = self._now()
            return [dict(row)]
        if head == "DELETE":
            count = len(self.rows)
            self.rows.clear()
            return [{"channel_id": "x"}] * count
        return []

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        return self.rows.get(str(args[0]))

    async def fetchval(self, query: str, *args: Any) -> Any:
        row = self.rows.get(str(args[0]))
        if row is None:
            return 0
        row["message_count"] += 1
        return row["message_count"]


class _RealSQL:
    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._db: Any = None

    async def _pool(self) -> Any:
        if self._db is None:
            from crewlet.db.client import Database

            self._db = await Database.connect(self._dsn)
            await self._db.execute("DELETE FROM a2a_channels")
        return self._db

    async def aclose(self) -> None:
        if self._db is not None:
            await self._db.close()
            self._db = None

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        return await (await self._pool()).execute(query, *args)

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        return await (await self._pool()).fetchrow(query, *args)

    async def fetchval(self, query: str, *args: Any) -> Any:
        return await (await self._pool()).fetchval(query, *args)


BACKENDS = [
    pytest.param(MemoryA2AChannelStore, id="memory"),
    pytest.param(lambda: PostgresA2AChannelStore(_FakeSQL()), id="postgres"),
    pytest.param(
        lambda: PostgresA2AChannelStore(_RealSQL(_TEST_DSN)),
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


async def test_an_opened_channel_reads_back(store: Any) -> None:
    await store.open("c1", requester="alice", target="bob")
    channel = await store.get("c1")
    assert channel is not None
    assert channel.requester == "alice"
    assert channel.target == "bob"
    assert channel.is_open


async def test_an_unknown_channel_is_none_not_closed(store: Any) -> None:
    """Two different answers a caller must not conflate: one is a typo,
    the other is a conversation that ended."""
    assert await store.get("never-opened") is None


async def test_other_party_answers_from_either_side(store: Any) -> None:
    """Routing a reply is exactly this question, and it is asked on the
    node that owns the ANSWERING seat — which knows neither party until
    it reads them here."""
    await store.open("c1", requester="alice", target="bob")
    channel = await store.get("c1")
    assert channel is not None
    assert channel.other_party("alice") == "bob"
    assert channel.other_party("bob") == "alice"
    assert channel.other_party("mallory") == ""


async def test_closing_is_idempotent_and_returns_the_closed_row(store: Any) -> None:
    """Both sides may reasonably try to close. Only one call may be the
    one that closed it, or the closed event is published twice.

    The winner gets the ROW, not a bool: the caller publishes the
    conversation's closing event, and the row is what names its parties,
    its message count and when it closed.
    """
    await store.open("c1", requester="alice", target="bob")
    closed = await store.close("c1")
    assert closed is not None
    assert closed.participants == ["alice", "bob"]
    assert closed.closed_at is not None
    assert await store.close("c1") is None
    channel = await store.get("c1")
    assert channel is not None and not channel.is_open


async def test_closing_an_unknown_channel_is_none_not_an_error(store: Any) -> None:
    assert await store.close("never-opened") is None


async def test_a_closed_channel_can_report_how_long_it_was_open(store: Any) -> None:
    """The duration used to be hardcoded to zero at every call site, so
    the dashboard rendered "0.0s" for every A2A channel ever.

    Both endpoints come from the store, so they are two readings of ONE
    clock — a channel is opened on one node and closed on another as a
    matter of course, and the difference of two nodes' clocks is skew,
    not a duration.
    """
    await store.open("c1", requester="alice", target="bob")
    open_channel = await store.get("c1")
    assert open_channel is not None
    assert open_channel.duration_ms == 0.0, "an open channel has no duration yet"

    closed = await store.close("c1")
    assert closed is not None
    assert closed.duration_ms >= 0.0
    assert closed.closed_at is not None and closed.opened_at <= closed.closed_at


async def test_message_counting_accumulates(store: Any) -> None:
    await store.open("c1", requester="alice", target="bob")
    assert await store.count_message("c1") == 1
    assert await store.count_message("c1") == 2
    channel = await store.get("c1")
    assert channel is not None and channel.message_count == 2


async def test_close_idle_closes_only_open_channels(store: Any) -> None:
    """An abandoned channel is a row that never goes away AND a promise
    to the requester that a reply may still arrive. Everything already
    closed is left alone, so the sweep cannot re-close and re-announce."""
    await store.open("stale", requester="alice", target="bob")
    await store.open("done", requester="alice", target="carol")
    await store.close("done")

    closed = await store.close_idle(older_than_seconds=-1)
    assert [c.channel_id for c in closed] == ["stale"]
    # The rows, not the ids: the sweep publishes a closed event per
    # channel and these are precisely the ones worth tracing — a channel
    # only reaches the sweep because the turn that should have answered
    # it never finished.
    assert closed[0].participants == ["alice", "bob"]
    assert closed[0].closed_at is not None
    assert await store.close_idle(older_than_seconds=-1) == []


async def test_close_idle_respects_the_interval(store: Any) -> None:
    """A channel younger than the timeout is one somebody may still be
    answering on — closing it breaks the promise the sweep exists to
    keep, and does so on a healthy conversation."""
    await store.open("fresh", requester="alice", target="bob")
    assert await store.close_idle(older_than_seconds=3600) == []
    channel = await store.get("fresh")
    assert channel is not None and channel.is_open


async def test_purge_deletes_rows() -> None:
    store = MemoryA2AChannelStore()
    await store.open("c1", requester="alice", target="bob")
    assert await store.purge(older_than_seconds=-1) == 1
    assert await store.get("c1") is None
