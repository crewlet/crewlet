"""Tests for the SandboxWaiter completion poll + pause reaper."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from typing import Any

from crewlet.queue.topics import agent_control_topic
from crewlet.sandbox import FakeCodingAgentRunner, FakeSandboxProvider, SandboxManager
from crewlet.sandbox.coordinator import COMPLETIONS_TOPIC
from crewlet.sandbox.pending_store import (
    MemoryPendingSandboxRunStore,
    PendingSandboxRun,
)
from crewlet.sandbox.waiter import SandboxWaiter


@dataclass
class _QueueStub:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


def _mk_manager(poll_done: bool):
    runner = FakeCodingAgentRunner(name="claude-code", poll_done=poll_done)
    provider = FakeSandboxProvider()
    return SandboxManager(provider=provider, runners={"claude-code": runner}), provider


def _run(**over: Any) -> PendingSandboxRun:
    base: dict[str, Any] = {
        "turn_id": "t-1",
        "agent_handle": "eng",
        "agent_id": "aid",
        "role": "Engineer",
        "sandbox_id": "sbx-1",
        "coding_agent": "claude-code",
        "command_id": "cmd-1",
        "trace_id": "trace-1",
        "span_id": "span-1",
    }
    base.update(over)
    return PendingSandboxRun(**base)


async def test_tick_fires_completion_when_job_done() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    manager, provider = _mk_manager(poll_done=True)
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    queue = _QueueStub()
    waiter = SandboxWaiter(event_queue=queue, pending_store=store, manager=manager)

    fired = await waiter.tick_once()

    assert fired == 1
    # Two publishes, two purposes. The ``crewlet.events.*`` copy is an
    # ANNOUNCEMENT the dashboard's broadcast stream watches; the per-seat
    # control copy is a COMMAND, routed to whoever holds the seat,
    # because only that node has the suspended Execute conversation.
    topics = [t for t, _e in queue.published]
    assert topics == [COMPLETIONS_TOPIC, agent_control_topic("eng")]
    for _topic, event in queue.published:
        assert event.type == "sandbox_run_completed"
        assert event.turn_id == "t-1"
        assert event.agent_handle == "eng"
        # The original trace rides along so the completion turn nests.
        assert event.trace_id == "trace-1"


async def test_tick_no_fire_when_job_still_running() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    manager, provider = _mk_manager(poll_done=False)
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    queue = _QueueStub()
    waiter = SandboxWaiter(event_queue=queue, pending_store=store, manager=manager)

    assert await waiter.tick_once() == 0
    assert queue.published == []


async def test_tick_keepalives_running_box() -> None:
    # No run-time TTL: each tick refreshes a running box's kill timer (to the
    # manager's keepalive window) so the clock never reaps a live job. The
    # window rolls — a second tick refreshes again.
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    manager, provider = _mk_manager(poll_done=False)  # job still running
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    queue = _QueueStub()
    waiter = SandboxWaiter(event_queue=queue, pending_store=store, manager=manager)

    assert await waiter.tick_once() == 0  # not done — just keepalive
    assert provider.sandboxes[0].timeout_refreshes == [manager.default_timeout_s]
    assert await waiter.tick_once() == 0
    assert provider.sandboxes[0].timeout_refreshes == [
        manager.default_timeout_s,
        manager.default_timeout_s,
    ]


async def test_tick_skips_non_running_rows() -> None:
    store = MemoryPendingSandboxRunStore()
    await store.create(_run(turn_id="t-clar"))
    await store.mark_awaiting_clarification(
        "t-clar", question="Q", audience="team", conversation_key="k"
    )
    manager, provider = _mk_manager(poll_done=True)
    queue = _QueueStub()
    waiter = SandboxWaiter(event_queue=queue, pending_store=store, manager=manager)

    # The only active row is awaiting_clarification → not polled.
    assert await waiter.tick_once() == 0
    assert queue.published == []


async def test_tick_survives_a_vanished_sandbox() -> None:
    # connect() raises (TTL death) → logged, no crash, no completion on the
    # first tick (a transient blip must not fail the run).
    store = MemoryPendingSandboxRunStore()
    await store.create(_run(sandbox_id="gone"))
    manager, _ = _mk_manager(poll_done=True)  # provider has no "gone" id
    queue = _QueueStub()
    waiter = SandboxWaiter(event_queue=queue, pending_store=store, manager=manager)

    assert await waiter.tick_once() == 0
    assert queue.published == []


async def test_tick_reaps_sandbox_gone_after_repeated_failures() -> None:
    # A sandbox that never comes back (TTL death) must not orphan the run:
    # after enough consecutive reconnect failures the waiter fires anyway so
    # the coordinator frees the agent + marks the run failed.
    from crewlet.sandbox.waiter import _MAX_CONNECT_FAILURES

    store = MemoryPendingSandboxRunStore()
    await store.create(_run(sandbox_id="gone"))
    manager, _ = _mk_manager(poll_done=True)  # provider has no "gone" id
    queue = _QueueStub()
    waiter = SandboxWaiter(event_queue=queue, pending_store=store, manager=manager)

    # The failures just below the threshold do not fire.
    for _ in range(_MAX_CONNECT_FAILURES - 1):
        assert await waiter.tick_once() == 0
    assert queue.published == []
    # The threshold tick reaps it.
    assert await waiter.tick_once() == 1
    topic, event = queue.published[0]
    assert topic == COMPLETIONS_TOPIC
    assert event.turn_id == "t-1"


async def test_reaps_paused_clarification_past_pause_ttl() -> None:
    # THE reason the reaper exists: a run blocked on a person's answer parks
    # its sandbox PAUSED, and a paused E2B box has no provider-side TTL — it
    # is held (and billed) forever. Past pause_ttl the waiter kills it and
    # flips the run to `reseed`; the answer can still arrive and resumes the
    # work from the pushed branch.
    store = MemoryPendingSandboxRunStore()
    run = _run(pause_ttl_seconds=1800.0)
    await store.create(run)
    await store.mark_awaiting_clarification(
        "t-1", question="Q", audience="team", conversation_key="k"
    )
    run.paused_at = datetime.now(UTC) - timedelta(seconds=1801)
    manager, provider = _mk_manager(poll_done=True)
    box = await provider.create(manager.build_spec())
    await box.pause()  # as collect_sandbox_run left it
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    queue = _QueueStub()
    waiter = SandboxWaiter(event_queue=queue, pending_store=store, manager=manager)

    assert await waiter.tick_once() == 0  # a reap is not a completion

    assert provider.sandboxes[0].closed is True
    # Killed by id — never un-paused first (connect() would resume the VM
    # just to shut it down).
    assert provider.sandboxes[0].paused is True
    row = await store.get("t-1")
    assert row is not None
    assert row.status == "reseed"
    # Released, so the eventual answer provisions a FRESH box rather than
    # reattaching to a dead sandbox id.
    assert row.sandbox_id == ""
    assert row.paused_at is None
    assert queue.published == []


async def test_does_not_reap_a_pause_still_inside_its_ttl() -> None:
    store = MemoryPendingSandboxRunStore()
    run = _run(pause_ttl_seconds=1800.0)
    await store.create(run)
    await store.mark_awaiting_clarification(
        "t-1", question="Q", audience="team", conversation_key="k"
    )
    run.paused_at = datetime.now(UTC) - timedelta(seconds=60)
    manager, provider = _mk_manager(poll_done=True)
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    waiter = SandboxWaiter(
        event_queue=_QueueStub(), pending_store=store, manager=manager
    )

    await waiter.tick_once()

    assert provider.sandboxes[0].closed is False
    row = await store.get("t-1")
    assert row is not None and row.status == "awaiting_clarification"
    assert row.sandbox_id == "sbx-1"


async def test_does_not_reap_a_box_owned_by_a_live_tail() -> None:
    # `resumed` means a completion is being collected / an Execute loop is
    # resuming. That box is paused too, but a tail is actively driving it and
    # will settle it within the turn — reaping from here would pull the box
    # out from under live work.
    store = MemoryPendingSandboxRunStore()
    run = _run(pause_ttl_seconds=1800.0)
    await store.create(run)
    await store.set_status("t-1", "resumed")
    run.paused_at = datetime.now(UTC) - timedelta(days=1)
    manager, provider = _mk_manager(poll_done=True)
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    waiter = SandboxWaiter(
        event_queue=_QueueStub(), pending_store=store, manager=manager
    )

    await waiter.tick_once()

    assert provider.sandboxes[0].closed is False
    row = await store.get("t-1")
    assert row is not None and row.status == "resumed"


async def test_reap_releases_the_row_even_when_the_kill_fails() -> None:
    # An old snapshot the provider has already dropped is the normal case;
    # the row must still stop tracking a dead id, or the run is stuck
    # pointing at a box that will never come back.
    store = MemoryPendingSandboxRunStore()
    run = _run(sandbox_id="already-gone", pause_ttl_seconds=60.0)
    await store.create(run)
    await store.mark_awaiting_clarification(
        "t-1", question="Q", audience="team", conversation_key="k"
    )
    run.paused_at = datetime.now(UTC) - timedelta(seconds=600)
    manager, provider = _mk_manager(poll_done=True)

    async def _boom(sandbox_id: str) -> None:
        raise RuntimeError("sandbox not found")

    provider.kill = _boom  # type: ignore[method-assign]
    waiter = SandboxWaiter(
        event_queue=_QueueStub(), pending_store=store, manager=manager
    )

    await waiter.tick_once()

    row = await store.get("t-1")
    assert row is not None and row.status == "reseed" and row.sandbox_id == ""


async def test_reaper_ignores_a_run_that_never_paused() -> None:
    # pause_ttl_seconds == 0 is the "never pause, always re-seed" opt-out:
    # collect already closed the box, so there is no snapshot to expire and
    # nothing for the reaper to do.
    store = MemoryPendingSandboxRunStore()
    run = _run(sandbox_id="", pause_ttl_seconds=0.0)
    await store.create(run)
    await store.mark_awaiting_clarification(
        "t-1", question="Q", audience="team", conversation_key="k"
    )
    manager, provider = _mk_manager(poll_done=True)
    waiter = SandboxWaiter(
        event_queue=_QueueStub(), pending_store=store, manager=manager
    )

    await waiter.tick_once()

    row = await store.get("t-1")
    assert row is not None and row.status == "awaiting_clarification"


async def test_transient_connect_failure_resets_on_success() -> None:
    # A connect blip followed by a successful reconnect must NOT count toward
    # the gone-threshold — the streak resets, so a live-but-flaky sandbox is
    # never wrongly reaped.
    store = MemoryPendingSandboxRunStore()
    await store.create(_run())
    manager, provider = _mk_manager(poll_done=False)  # job still running
    await provider.create(manager.build_spec())
    provider._by_id["sbx-1"] = provider.sandboxes[0]
    queue = _QueueStub()
    waiter = SandboxWaiter(event_queue=queue, pending_store=store, manager=manager)

    waiter._connect_failures["t-1"] = 3  # simulate a prior streak
    assert await waiter.tick_once() == 0  # connect succeeds → streak cleared
    assert "t-1" not in waiter._connect_failures


async def test_an_answer_that_lands_mid_reap_keeps_its_box() -> None:
    """The reaper decides from a snapshot taken seconds earlier.

    In that gap the person can answer: ``claim_for_resume`` flips the row
    to ``resumed`` and the Execute loop reconnects to this very box. The
    compare-and-set is what catches it — so it has to gate the KILL, not
    just the status write. Killing first destroyed the box underneath the
    live resume and then the CAS refused, so the reaper walked away
    silently and the answered run failed on a box that no longer existed.
    """
    import dataclasses

    store = MemoryPendingSandboxRunStore()
    run = _run(pause_ttl_seconds=1800.0)
    await store.create(run)
    await store.mark_awaiting_clarification(
        "t-1", question="Q", audience="team", conversation_key="k"
    )
    run.paused_at = datetime.now(UTC) - timedelta(seconds=1801)
    stale = dataclasses.replace(run)  # what the tick read, before the answer

    manager, provider = _mk_manager(poll_done=True)
    box = await provider.create(manager.build_spec())
    await box.pause()
    provider._by_id["sbx-1"] = provider.sandboxes[0]

    # The answer lands after the snapshot and before the reap decision.
    assert await store.claim_for_resume("t-1") is not None

    async def _stale_list() -> list[PendingSandboxRun]:
        return [stale]

    store.list_active = _stale_list  # type: ignore[method-assign]
    waiter = SandboxWaiter(
        event_queue=_QueueStub(), pending_store=store, manager=manager
    )

    await waiter.tick_once()

    assert provider.sandboxes[0].closed is False, (
        "the reaper killed a box the resumed run is reconnecting to"
    )
    row = await store.get("t-1")
    assert row is not None
    assert row.status == "resumed"
    assert row.sandbox_id == "sbx-1"
