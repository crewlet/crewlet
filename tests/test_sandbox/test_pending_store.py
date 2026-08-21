"""Tests for the pending-sandbox-run store.

Covers the in-memory store's full state-machine semantics (the
at-most-once resume flip, clarification parking, conversation-key
matching, active-recovery listing) and pins the Postgres store's SQL
shape against a fake DB.

The fake-DB cases assert the statement's WORDING, which is a useful
regression guard and nothing more: a statement PostgreSQL refuses to
parse passes every one of them, and the at-most-once claim's exclusivity
— the whole correctness argument for resuming a detached run — was
"proved" by a fake returning the value the test had just put in a dict.
So the section at the bottom runs the same store against a real server
whenever CREWLET_TEST_DSN is set, and asserts BEHAVIOUR instead.
"""

from __future__ import annotations

import os
from typing import Any

import pytest

from crewlet.sandbox.pending_store import (
    MemoryPendingSandboxRunStore,
    PendingSandboxRun,
    PostgresPendingSandboxRunStore,
)

_TEST_DSN = os.environ.get("CREWLET_TEST_DSN", "")


def _run(**over: Any) -> PendingSandboxRun:
    base: dict[str, Any] = {
        "turn_id": "t-1",
        "agent_handle": "eng",
        "sandbox_id": "sbx-1",
        "coding_agent": "claude-code",
        "plan": {"reasoning": "do the code work"},
        "success_criteria": ["PR opened"],
        "conversation_key": "slack:C1:99",
    }
    base.update(over)
    return PendingSandboxRun(**base)


# --- memory store: state machine ------------------------------------------


async def test_create_get_roundtrip() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    got = await store.get("t-1")
    assert got is not None
    assert got.agent_handle == "eng"
    assert got.success_criteria == ["PR opened"]
    assert got.status == "running"


async def test_create_is_idempotent() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run(status="running"))
    # A republished kick-off must not clobber an already-advanced row.
    await store.create(_run(status="done"))
    got = await store.get("t-1")
    assert got is not None and got.status == "running"


async def test_claim_for_resume_is_at_most_once() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    first = await store.claim_for_resume("t-1")
    assert first is not None and first.status == "resumed"
    # Second claim (duplicate completion signal) loses.
    assert await store.claim_for_resume("t-1") is None


async def test_claim_for_resume_works_from_clarification() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    await store.mark_awaiting_clarification(
        "t-1", question="Which?", audience="team", conversation_key="slack:C1:99"
    )
    claimed = await store.claim_for_resume("t-1")
    assert claimed is not None
    # The claimed row carries the question → caller knows it's an answer.
    assert claimed.question == "Which?"


async def test_mark_awaiting_only_from_running() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    assert (
        await store.mark_awaiting_clarification(
            "t-1", question="Q", audience="requester", conversation_key="k"
        )
        is True
    )
    # Not running anymore → second flip refused.
    assert (
        await store.mark_awaiting_clarification(
            "t-1", question="Q2", audience="team", conversation_key="k"
        )
        is False
    )


async def test_find_awaiting_by_conversation() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    # Not yet awaiting → no match.
    assert await store.find_awaiting_by_conversation("slack:C1:99") is None
    await store.mark_awaiting_clarification(
        "t-1", question="Q", audience="team", conversation_key="slack:C1:99"
    )
    found = await store.find_awaiting_by_conversation("slack:C1:99")
    assert found is not None and found.turn_id == "t-1"
    assert await store.find_awaiting_by_conversation("") is None


async def test_list_active_excludes_terminal() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run(turn_id="t-run"))
    await store.create(_run(turn_id="t-clar"))
    await store.create(_run(turn_id="t-done"))
    await store.create(_run(turn_id="t-mid"))
    await store.mark_awaiting_clarification(
        "t-clar", question="Q", audience="team", conversation_key="k"
    )
    await store.set_status("t-done", "done")
    # `resumed` stays active: it is the only way boot recovery can SEE a tail
    # that died mid-flight, whose paused box would otherwise leak forever.
    await store.set_status("t-mid", "resumed")
    active = {r.turn_id for r in await store.list_active()}
    assert active == {"t-run", "t-clar", "t-mid"}


async def test_reaped_run_stays_claimable_and_conversation_matched() -> None:
    # Reaping a paused box past pause_ttl does NOT end the run — the person's
    # answer can still arrive days later and resume the suspended Execute
    # loop, so `reseed` must remain both matchable and claimable.
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    await store.mark_awaiting_clarification(
        "t-1", question="Q", audience="team", conversation_key="slack:C1:99"
    )
    await store.release_box("t-1")
    await store.set_status("t-1", "reseed")

    found = await store.find_awaiting_by_conversation("slack:C1:99")
    assert found is not None and found.turn_id == "t-1"
    claimed = await store.claim_for_resume("t-1")
    assert claimed is not None and claimed.claimed_from == "reseed"
    assert claimed.status == "resumed"


async def test_box_state_transitions() -> None:
    # The row is the engine's record of the box: sandbox_id non-empty means
    # a box exists, paused_at set means it is paused. Every reaper decision
    # reads exactly those two fields.
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())

    await store.mark_box_paused("t-1")
    row = await store.get("t-1")
    assert row is not None and row.sandbox_id == "sbx-1" and row.paused_at is not None

    await store.release_box("t-1")
    row = await store.get("t-1")
    assert row is not None and row.sandbox_id == "" and row.paused_at is None

    # A re-seeded launch re-points the SAME row at a fresh box + command.
    await store.set_status("t-1", "reseed")
    await store.attach_sandbox(
        "t-1", sandbox_id="sbx-2", command_id="cmd-2", session_id="sess-2"
    )
    row = await store.get("t-1")
    assert row is not None
    assert (row.sandbox_id, row.command_id, row.session_id) == (
        "sbx-2",
        "cmd-2",
        "sess-2",
    )
    assert row.status == "running" and row.paused_at is None
    # The original framing survives the re-point.
    assert row.conversation_key == "slack:C1:99"


async def test_delete() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    await store.delete("t-1")
    assert await store.get("t-1") is None


# --- postgres store: SQL shape --------------------------------------------


class _FakeDB:
    def __init__(self, *, rows: dict[str, Any] | None = None) -> None:
        self.calls: list[tuple[str, tuple[Any, ...]]] = []
        self._rows = rows or {}

    async def execute(self, query: str, *args: Any) -> list[dict[str, Any]]:
        self.calls.append((query, args))
        return []

    async def fetchrow(self, query: str, *args: Any) -> dict[str, Any] | None:
        self.calls.append((query, args))
        if "UPDATE" in query and "resumed" in query:
            return self._rows.get("claim")
        if "SELECT" in query:
            return self._rows.get("get")
        if "UPDATE" in query:
            return self._rows.get("update")
        return None


async def test_pg_create_sql() -> None:
    db = _FakeDB()
    store = PostgresPendingSandboxRunStore(db)  # type: ignore[arg-type]
    await store.create(_run())
    query, args = db.calls[0]
    assert "INSERT INTO pending_sandbox_run" in query
    assert "ON CONFLICT (turn_id) DO NOTHING" in query
    assert "t-1" in args  # turn_id bound


async def test_pg_claim_for_resume_flip_sql() -> None:
    row = {"turn_id": "t-1", "agent_handle": "eng", "status": "resumed"}
    db = _FakeDB(rows={"claim": row})
    store = PostgresPendingSandboxRunStore(db)  # type: ignore[arg-type]
    claimed = await store.claim_for_resume("t-1")
    assert claimed is not None and claimed.turn_id == "t-1"
    query, _ = db.calls[0]
    assert "SET status = 'resumed'" in query
    # 'reseed' is claimable too: reaping a run's paused box past pause_ttl
    # does not end the run — the answer can still arrive and resume it.
    assert "IN ('running', 'awaiting_clarification', 'reseed')" in query
    assert "RETURNING" in query


async def test_pg_claim_for_resume_conflict_returns_none() -> None:
    db = _FakeDB(rows={"claim": None})  # no row → already claimed
    store = PostgresPendingSandboxRunStore(db)  # type: ignore[arg-type]
    assert await store.claim_for_resume("t-1") is None


async def test_pg_box_state_sql() -> None:
    db = _FakeDB()
    store = PostgresPendingSandboxRunStore(db)  # type: ignore[arg-type]

    await store.mark_box_paused("t-1")
    assert "paused_at = now()" in db.calls[0][0]

    await store.release_box("t-1")
    query = db.calls[1][0]
    assert "sandbox_id = ''" in query and "paused_at = NULL" in query
    # Status is deliberately untouched: the reaper releases the box and THEN
    # decides the run is `reseed`; teardown releases it and THEN marks `done`.
    assert "status" not in query

    await store.attach_sandbox(
        "t-1", sandbox_id="sbx-2", command_id="cmd-2", session_id="sess-2"
    )
    query, args = db.calls[2]
    assert "status = 'running'" in query and "paused_at = NULL" in query
    assert "sbx-2" in args and "cmd-2" in args


async def test_pg_find_awaiting_matches_reaped_runs_too() -> None:
    db = _FakeDB()
    store = PostgresPendingSandboxRunStore(db)  # type: ignore[arg-type]
    await store.find_awaiting_by_conversation("slack:C1:99")
    query, _ = db.calls[0]
    assert "IN ('awaiting_clarification', 'reseed')" in query


async def test_pg_list_active_includes_resumed() -> None:
    # Boot recovery needs to see a tail abandoned by a dead engine, or its
    # paused box leaks with nothing left to reclaim it.
    db = _FakeDB()
    store = PostgresPendingSandboxRunStore(db)  # type: ignore[arg-type]
    await store.list_active()
    query, _ = db.calls[0]
    assert "'resumed'" in query


# ── the real server ──────────────────────────────────────────────────
#
# Behaviour, not wording. Everything above this line runs against a fake
# that answers from a dict, so none of it can tell a correct statement
# from one the server rejects — and the at-most-once claim is the
# primitive that decides whether a detached coding run resumes once or
# twice.


@pytest.fixture
async def real_store() -> Any:
    if not _TEST_DSN:
        pytest.skip("set CREWLET_TEST_DSN to run against real PG")
    from crewlet.db.client import Database

    db = await Database.connect(_TEST_DSN)
    await db.execute("DELETE FROM pending_sandbox_run")
    try:
        yield PostgresPendingSandboxRunStore(db)
    finally:
        await db.close()


@pytest.mark.integration
async def test_real_claim_for_resume_is_at_most_once(real_store: Any) -> None:
    """The primitive the whole resume path rests on.

    Two completion signals for one run — successive poll ticks, or an
    at-least-once redelivery — must not both resume the suspended Execute
    loop. Only the statement can enforce that, and until now only a fake
    had ever been asked.
    """
    await real_store.create(_run())
    first = await real_store.claim_for_resume("t-1")
    second = await real_store.claim_for_resume("t-1")
    assert first is not None and first.turn_id == "t-1"
    assert second is None, "a second signal claimed a run that was already claimed"


@pytest.mark.integration
async def test_real_round_trip_preserves_the_suspended_conversation(
    real_store: Any,
) -> None:
    """``execute_state`` IS the suspended Execute loop. A round trip that
    loses it turns a resumable run into a lost turn."""
    state = {
        "messages": [{"role": "system", "content": "EXECUTE"}],
        "pending_tool_call_id": "c1",
        "pending_tool_name": "run_sandbox",
        "active_tool_names": [],
        "loaded_skill_keys": [],
        "iteration": 2,
    }
    await real_store.create(_run(execute_state=state))
    got = await real_store.get("t-1")
    assert got is not None
    assert got.execute_state == state
    assert got.success_criteria == ["PR opened"]
    assert got.conversation_key == "slack:C1:99"


@pytest.mark.integration
async def test_real_a_stale_epoch_cannot_write(real_store: Any) -> None:
    """The fence. A node that lost the seat mid-run must not be able to
    move the row under the node that owns it now."""
    await real_store.create(_run())
    assert await real_store.claim_ownership("t-1", owner="node-a:1111", epoch=3)

    await real_store.set_status("t-1", "failed", epoch=2)
    got = await real_store.get("t-1")
    assert got is not None and got.status != "failed", (
        "a write fenced at a stale epoch landed anyway"
    )

    await real_store.set_status("t-1", "failed", epoch=3)
    got = await real_store.get("t-1")
    assert got is not None and got.status == "failed"


@pytest.mark.integration
async def test_real_a_parked_run_is_found_by_its_conversation(
    real_store: Any,
) -> None:
    """How an answer reaches the run that asked: the next inbound on the
    conversation is matched back to the parked row."""
    await real_store.create(_run())
    await real_store.mark_awaiting_clarification(
        "t-1", question="which branch?", audience="ops", conversation_key="slack:C1:99"
    )
    found = await real_store.find_awaiting_by_conversation("slack:C1:99")
    assert found is not None and found.turn_id == "t-1"
    assert await real_store.find_awaiting_by_conversation("slack:OTHER:1") is None


@pytest.mark.integration
async def test_real_active_listing_drives_restart_recovery(real_store: Any) -> None:
    """On boot the coordinator re-pauses the inboxes of agents with live
    runs from this list. A run missing from it is a seat that never gets
    its busy state back."""
    await real_store.create(_run())
    await real_store.create(_run(turn_id="t-2", agent_handle="ops"))
    assert {r.turn_id for r in await real_store.list_active()} == {"t-1", "t-2"}
    assert {r.turn_id for r in await real_store.list_active_for_seat("eng")} == {"t-1"}

    await real_store.delete("t-2")
    assert {r.turn_id for r in await real_store.list_active()} == {"t-1"}


@pytest.mark.integration
async def test_real_releasing_the_box_clears_the_row_it_is_recorded_on(
    real_store: Any,
) -> None:
    """``sandbox_id`` non-empty IS the box record, so a torn-down box has
    to clear it — a stale id is something a later pass tries to reattach
    to."""
    await real_store.create(_run())
    await real_store.mark_box_paused("t-1")
    got = await real_store.get("t-1")
    assert got is not None and got.paused_at is not None

    await real_store.release_box("t-1")
    got = await real_store.get("t-1")
    assert got is not None and got.sandbox_id == ""
    assert got.paused_at is None
