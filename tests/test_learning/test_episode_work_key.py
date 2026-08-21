"""One turn, one episode — even when two nodes both finish it.

The duplicate this file exists for is the least visible harm the design
admits to. A doubled Slack post is obvious and a human deletes it; a
doubled *episode* is retrieved by every later turn's recall and feeds
skill synthesis, so it quietly reweights an agent's behaviour with
something that happened once.

Two paths produce it, neither a defect on its own: a zombie whose loop
stalls past its lease and completes between fence checks, and a genuine
re-run after the completion ledger fails open — which it does on
purpose, because "I cannot tell whether this was done" has exactly one
safe answer.

Run against a real database (``CREWLET_TEST_DSN``) as well as a fake,
because the guard IS the SQL. An ``INSERT … SELECT … WHERE NOT EXISTS``
under an advisory lock is not something to trust unrun: without the
lock the ``NOT EXISTS`` is evaluated under the statement's snapshot and
both writers insert, which is measured below rather than asserted.
"""

from __future__ import annotations

import asyncio
import os
from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

import pytest

from crewlet.learning.episode_store import EpisodeStore, _lock_word
from crewlet.learning.models import Episode

_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")

pytestmark = pytest.mark.skipif(
    not _TEST_DSN, reason="set CREWLET_TEST_DSN to run against real PG"
)

# The real table's columns, minus the hypertable. `create_hypertable`
# needs TimescaleDB, and partitioning plays no part in the exclusion —
# it is in fact WHY the exclusion is not a unique index (a hypertable
# requires the partition column in every one, and the two duplicate
# turns end at different times). Everything the guard touches is here,
# with the real types, so the store's own SQL runs unmodified.
_DROP = "DROP TABLE IF EXISTS episodes"

# One statement per call: asyncpg prepares every query, and a prepared
# statement cannot carry multiple commands.
_CREATE = """
CREATE TABLE episodes (
    id              UUID        NOT NULL DEFAULT gen_random_uuid(),
    agent_handle    TEXT        NOT NULL,
    agent_role      TEXT        NOT NULL DEFAULT '',
    task_id         TEXT        NOT NULL DEFAULT '',
    turn_id         UUID        NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    ended_at        TIMESTAMPTZ NOT NULL,
    plan_summary    TEXT        NOT NULL DEFAULT '',
    task_summary    TEXT        NOT NULL DEFAULT '',
    tool_sequence   JSONB       NOT NULL DEFAULT '[]'::jsonb,
    skills_used     JSONB       NOT NULL DEFAULT '[]'::jsonb,
    review_outcome  TEXT        NOT NULL DEFAULT 'done',
    duration_ms     INTEGER     NOT NULL DEFAULT 0,
    embedding       vector(1536),
    kind            TEXT        NOT NULL DEFAULT 'raw',
    count           INTEGER     NOT NULL DEFAULT 1,
    exemplar_turn_ids JSONB     NOT NULL DEFAULT '[]'::jsonb,
    consolidated_into_skill_id UUID,
    common_task_pattern TEXT    NOT NULL DEFAULT '',
    common_outcome  TEXT        NOT NULL DEFAULT '',
    success_rate    REAL,
    subjects_involved JSONB     NOT NULL DEFAULT '[]'::jsonb,
    notable_patterns TEXT       NOT NULL DEFAULT '',
    work_key        TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (ended_at, id)
)
"""

_INDEX = """
CREATE INDEX episodes_agent_work_key_idx
    ON episodes (agent_handle, work_key) WHERE work_key <> ''
"""


class _NoEmbeddings:
    """The store embeds before it inserts; the guard is downstream."""

    async def embed(self, texts: list[str]) -> list[list[float]]:
        return []

    async def embed_one(self, text: str) -> list[float]:
        return []


def _episode(handle: str = "eng", *, work_key: str) -> Episode:
    now = datetime.now(UTC)
    return Episode(
        agent_handle=handle,
        agent_role="Engineer",
        turn_id=uuid4(),  # DIFFERENT per call — two nodes mint two turns
        started_at=now,
        ended_at=now,
        plan_summary="p",
        task_summary="t",
        review_outcome="done",
        duration_ms=1,
        work_key=work_key,
    )


@pytest.fixture
async def store() -> Any:
    from crewlet.db.client import Database

    db = await Database.connect(_TEST_DSN)
    for statement in (_DROP, _CREATE, _INDEX):
        await db.execute(statement)
    try:
        yield EpisodeStore(db, _NoEmbeddings())
    finally:
        await db.execute(_DROP)
        await db.close()


async def _count(store: Any, work_key: str) -> int:
    rows = await store._db.execute(
        "SELECT count(*) AS n FROM episodes WHERE work_key = $1", work_key
    )
    return int(rows[0]["n"])


# ── the duplicate ────────────────────────────────────────────────────


async def test_two_turns_for_one_trigger_write_one_episode(store: Any) -> None:
    """The zombie, and the ledger's fail-open re-run. Two turns, two
    turn ids, one unit of work — so one memory of it."""
    await store.write(_episode(work_key="k1"))
    await store.write(_episode(work_key="k1"))
    assert await _count(store, "k1") == 1


async def test_the_first_writer_wins_rather_than_the_last(store: Any) -> None:
    """``DO NOTHING``, not ``DO UPDATE``: there is nothing to merge, and
    overwriting would let a zombie's version replace a good row."""
    first = _episode(work_key="k2")
    await store.write(first)
    await store.write(_episode(work_key="k2"))
    rows = await store._db.execute("SELECT turn_id FROM episodes WHERE work_key = 'k2'")
    assert str(rows[0]["turn_id"]) == str(first.turn_id)


async def test_different_agents_keep_their_own_memory(store: Any) -> None:
    """Two seats legitimately act on one trigger — a task assigned to a
    lead and its report. Each one's episode is its own."""
    await store.write(_episode("lead", work_key="shared"))
    await store.write(_episode("report", work_key="shared"))
    assert await _count(store, "shared") == 2


async def test_an_unkeyed_turn_is_never_deduplicated(store: Any) -> None:
    """A scheduled fire, a sub-agent, a sandbox resume. Hashing "no
    triggers" into a shared key would make a nightly job write one
    episode ever."""
    for _ in range(3):
        await store.write(_episode(work_key=""))
    assert await _count(store, "") == 3


async def test_two_concurrent_writers_still_write_one(store: Any) -> None:
    """The case the advisory lock exists for.

    ``WHERE NOT EXISTS`` alone is evaluated under the statement's
    snapshot, so two transactions that overlap can both see "not there"
    and both insert — the classic phantom, and exactly the shape of two
    nodes finishing the same turn at once.
    """
    await asyncio.gather(
        store.write(_episode(work_key="race")),
        store.write(_episode(work_key="race")),
    )
    assert await _count(store, "race") == 1


async def test_the_lock_word_is_stable_and_per_agent() -> None:
    """It has to be derivable identically on both nodes, or they take
    different locks and serialize against nobody."""
    assert _lock_word("eng", "k") == _lock_word("eng", "k")
    assert _lock_word("eng", "k") != _lock_word("ops", "k")
    # Postgres takes a signed 32-bit word.
    assert -(2**31) <= _lock_word("eng", "k") < 2**31


# ── the interaction counter ──────────────────────────────────────────

_CP_DROP = "DROP TABLE IF EXISTS counterparty_profiles"

_CP_CREATE = """
CREATE TABLE counterparty_profiles (
    observer_handle      TEXT        NOT NULL,
    subject_handle       TEXT        NOT NULL DEFAULT '',
    subject_external_id  TEXT        NOT NULL DEFAULT '',
    subject_platform     TEXT        NOT NULL DEFAULT '',
    subject_name         TEXT        NOT NULL DEFAULT '',
    traits               JSONB       NOT NULL DEFAULT '{}'::jsonb,
    interaction_count    INTEGER     NOT NULL DEFAULT 0,
    first_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_corroborated_at TIMESTAMPTZ,
    last_work_key        TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (observer_handle, subject_handle,
                 subject_external_id, subject_platform)
)
"""


@pytest.fixture
async def profiles() -> Any:
    from crewlet.db.client import Database
    from crewlet.learning.counterparty_store import CounterpartyStore

    db = await Database.connect(_TEST_DSN)
    for statement in (_CP_DROP, _CP_CREATE):
        await db.execute(statement)
    try:
        yield CounterpartyStore(db)
    finally:
        await db.execute(_CP_DROP)
        await db.close()


async def _observe(store: Any, work_key: str) -> int:
    profile = await store.upsert(
        observer_handle="eng",
        subject_handle="alice",
        traits_patch={"tone": "brisk"},
        work_key=work_key,
    )
    return profile.interaction_count


async def test_a_duplicate_turn_counts_one_interaction(profiles: Any) -> None:
    """The profiler reads conversational cadence off this number, and
    nothing ever decrements it — so a person counted twice looks twice
    as chatty as they are, permanently."""
    assert await _observe(profiles, "wk1") == 1
    assert await _observe(profiles, "wk1") == 1


async def test_the_guard_survives_the_very_first_write(profiles: Any) -> None:
    """The case that actually shipped broken.

    On the first write the row does not exist yet, so the INSERT branch
    runs — and if that branch does not seed ``last_work_key``, the
    duplicate's UPDATE compares '' against the key it is holding, finds
    no match, and counts twice. The guard then fails in precisely the
    situation it is guaranteed to meet: a brand-new counterparty.
    """
    await _observe(profiles, "first")
    rows = await profiles._db.execute(
        "SELECT last_work_key FROM counterparty_profiles WHERE observer_handle='eng'"
    )
    assert rows[0]["last_work_key"] == "first"


async def test_a_genuinely_new_interaction_still_counts(profiles: Any) -> None:
    await _observe(profiles, "wk1")
    assert await _observe(profiles, "wk2") == 2


async def test_an_unkeyed_observation_always_counts(profiles: Any) -> None:
    """A scheduled fire or a sub-agent has no cross-node duplicate, and
    must not silently stop counting."""
    assert await _observe(profiles, "") == 1
    assert await _observe(profiles, "") == 2
