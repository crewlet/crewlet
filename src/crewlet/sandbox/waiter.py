"""Completion poll + pause reaper for detached sandbox jobs.

This poll is THE completion signal: a periodic tick reconnects to each
still-``running`` sandbox by id and asks the runner whether its
background command has finished — covering a clean finish, a
finished-but-never-exited agent, a crashed process, and a sandbox that
vanished, so a detached run never hangs forever. There is deliberately
no push callback from inside the sandbox: only the poll can see a job
that died before reaching its last step, and the tick doubles as the
box keepalive (the ``set_timeout`` refresh below), so it must run at
this cadence regardless — a push signal could only shave less than one
``poll_seconds`` off jobs that run for minutes.

On detected completion it publishes :class:`SandboxRunCompleted` to the
engine-wide completions topic; the :class:`~crewlet.sandbox.coordinator.
SandboxCoordinator` does the at-most-once claim, collection, and the
resume of the suspended Execute loop. A duplicate signal is harmless —
successive ticks can both fire before the first claim lands, and queue
delivery is at-least-once, but the coordinator claims once.

The same tick is also the **pause reaper**. A run blocked on a
person's answer parks its sandbox *paused* so a quick reply resumes
exactly where the coding agent stopped — but a paused box has no
provider-side TTL (E2B holds it indefinitely and bills for the
snapshot), and the keepalive above deliberately doesn't touch it. Left
alone, one unanswered question strands a snapshot forever. So each tick
expires paused boxes older than their ``pause_ttl_seconds``: kill the
box by id (no resume — that would boot the VM to shut it down), release
it on the row, and flip the run to ``reseed``. The run is *not* over —
the answer can still arrive, and the work re-seeds from the pushed git
branch, which was always the durable state.

The loop mirrors the scheduler's lifecycle (``start`` / ``stop`` /
``_run_loop`` / ``tick_once``) so its start/stop wiring and
cancellation semantics are identical.
"""

from __future__ import annotations

import asyncio
import contextlib
from datetime import UTC, datetime
from typing import Any

from crewlet._logging import get_logger
from crewlet.events.types import SandboxRunCompleted
from crewlet.queue.topics import agent_control_topic
from crewlet.sandbox.coordinator import COMPLETIONS_TOPIC
from crewlet.sandbox.manager import SandboxManager
from crewlet.sandbox.pending_store import PendingSandboxRun, PendingSandboxRunStore
from crewlet.sandbox.protocol import RunHandle

logger = get_logger("sandbox.waiter")

# After this many CONSECUTIVE failed reconnects, the waiter gives up on a
# sandbox and fires completion anyway: the box is unreachable, so the run can
# never produce a result. The engine keeps a *running* box alive on every tick
# (the ``set_timeout`` keepalive below), so this never fires merely because a
# run is long — it means a genuine infra failure: E2B reclaimed the box, a network
# partition, or the engine was down long enough that the keepalive lapsed and
# E2B reaped the orphan. Firing lets the coordinator free the agent + mark the
# run failed instead of polling a dead sandbox forever. At the 15 s default
# this is ~1 min of grace for transient connect blips before declaring it gone.
_MAX_CONNECT_FAILURES = 4


class SandboxWaiter:
    """Polls running detached sandbox jobs and fires completion events."""

    def __init__(
        self,
        *,
        event_queue: Any,
        pending_store: PendingSandboxRunStore,
        manager: SandboxManager,
        # 15 s bounds the completion-detection latency (negligible against
        # coding jobs that run minutes) while a tick costs only one reconnect
        # + marker probe per running box; it also keeps the keepalive ~60x
        # inside the box TTL (``default_timeout_seconds``, 900 s) and makes
        # the give-up window ``_MAX_CONNECT_FAILURES`` * 15 s ≈ 1 min.
        poll_seconds: float = 15.0,
    ) -> None:
        self._event_queue = event_queue
        self._pending_store = pending_store
        self._manager = manager
        self._poll_seconds = poll_seconds
        self._running = False
        self._task: asyncio.Task[None] | None = None
        # turn_id -> consecutive reconnect failures (reset on any success).
        self._connect_failures: dict[str, int] = {}

    async def start(self) -> None:
        if self._running:
            return
        self._running = True
        loop = asyncio.get_event_loop()
        self._task = loop.create_task(self._run_loop(), name="sandbox_waiter")
        logger.info("sandbox_waiter_started", poll_seconds=self._poll_seconds)

    async def stop(self) -> None:
        if not self._running:
            return
        self._running = False
        task = self._task
        self._task = None
        if task is not None:
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await task
        logger.info("sandbox_waiter_stopped")

    async def _run_loop(self) -> None:
        try:
            while self._running:
                try:
                    await asyncio.sleep(self._poll_seconds)
                except asyncio.CancelledError:
                    return
                if not self._running:
                    return
                try:
                    await self.tick_once()
                except asyncio.CancelledError:
                    return
                except Exception:
                    logger.exception("sandbox_waiter_tick_failed")
        finally:
            self._running = False

    async def tick_once(self) -> int:
        """Poll every running run once; return how many completions fired.

        Also expires paused sandboxes past their ``pause_ttl`` (see
        :meth:`_reap_expired_pauses`). Exposed (like the scheduler's
        ``tick_once``) so tests drive a single pass deterministically
        without the sleep loop.
        """
        runs = await self._pending_store.list_active()
        active_ids = {r.turn_id for r in runs}
        # Drop failure counters for runs no longer active (claimed / terminal).
        self._connect_failures = {
            tid: n for tid, n in self._connect_failures.items() if tid in active_ids
        }
        fired = 0
        for run in runs:
            if run.status != "running":
                continue
            state = await self._poll_one(run)
            if state in ("done", "gone"):
                # "gone" → the sandbox vanished; fire anyway so the
                # coordinator frees the agent and marks the run failed
                # (collect will fail to reconnect) instead of hanging.
                await self._publish_completion(run)
                fired += 1
        if fired:
            logger.info("sandbox_waiter_fired", completions=fired)
        await self._reap_expired_pauses(runs)
        return fired

    async def _reap_expired_pauses(self, runs: list[PendingSandboxRun]) -> int:
        """Kill paused sandboxes past their ``pause_ttl``; return how many.

        Scoped to runs parked on a clarification: that is the one state
        whose box is deliberately held for an open-ended human wait, so it
        is the one that needs an engine-side expiry. Every other paused box
        belongs to a tail that is actively being driven (a completion being
        collected, an Execute loop resuming) and is settled by that tail
        within the turn — expiring those from here would kill a box out from
        under live work.

        ``pause_ttl_seconds == 0`` is not a deadline to enforce here: it
        means "never hold a blocked box", so the coordinator already tore
        this one down when the run blocked and there is no snapshot left to
        expire (the ``sandbox_id`` guard above catches it either way).
        """
        now = datetime.now(UTC)
        reaped = 0
        for run in runs:
            if run.status != "awaiting_clarification":
                continue
            if not run.sandbox_id or run.paused_at is None:
                continue
            if run.pause_ttl_seconds <= 0:
                continue
            paused_for = (now - run.paused_at).total_seconds()
            if paused_for < run.pause_ttl_seconds:
                continue
            # Kill by id: ``connect`` would auto-resume the snapshot, booting
            # the VM back up purely to shut it down.
            try:
                await self._manager.provider.kill(run.sandbox_id)
            except Exception:
                # An already-gone box is the normal case for an old snapshot;
                # release the row either way so we stop tracking a dead id.
                logger.warning(
                    "sandbox_pause_reap_kill_failed",
                    turn_id=run.turn_id,
                    sandbox_id=run.sandbox_id,
                )
            await self._pending_store.release_box(run.turn_id)
            await self._pending_store.set_status(run.turn_id, "reseed")
            reaped += 1
            logger.info(
                "sandbox_pause_ttl_reaped",
                turn_id=run.turn_id,
                agent=run.agent_handle,
                sandbox_id=run.sandbox_id,
                paused_for_s=round(paused_for, 1),
                pause_ttl_s=run.pause_ttl_seconds,
            )
        return reaped

    async def _poll_one(self, run: PendingSandboxRun) -> str:
        """Reconnect + ask the runner whether the job has finished.

        Returns ``"done"`` (finished), ``"running"`` (not yet / transient
        error — retry next tick), or ``"gone"`` (the sandbox has vanished
        across :data:`_MAX_CONNECT_FAILURES` consecutive reconnects, so the
        run can never complete and must be reaped).
        """
        try:
            sandbox = await self._manager.provider.connect(run.sandbox_id)
        except Exception:
            n = self._connect_failures.get(run.turn_id, 0) + 1
            self._connect_failures[run.turn_id] = n
            logger.warning(
                "sandbox_connect_failed",
                turn_id=run.turn_id,
                sandbox_id=run.sandbox_id,
                attempts=n,
            )
            return "gone" if n >= _MAX_CONNECT_FAILURES else "running"
        # Reconnect succeeded → clear any prior transient-failure streak.
        self._connect_failures.pop(run.turn_id, None)
        # Keepalive: the engine imposes NO run-time TTL on a coding job, so
        # refresh the box's kill timer every tick to keep a running job alive
        # for as long as it needs. The box is bounded only by how long the
        # engine can go *without* this heartbeat (orphan-reclaim grace), never
        # by a fixed run deadline. Completion is detected by tracking the job
        # (marker / terminal stream event / process liveness), not by a TTL.
        try:
            await sandbox.set_timeout(self._manager.default_timeout_s)
        except Exception:
            logger.debug("sandbox_keepalive_failed", turn_id=run.turn_id)
        try:
            runner = self._manager.runner_for(run.coding_agent)
            done = await runner.poll(
                sandbox,
                RunHandle(command_id=run.command_id, session_id=run.session_id),
            )
        except Exception:
            logger.exception(
                "sandbox_poll_failed", turn_id=run.turn_id, sandbox_id=run.sandbox_id
            )
            return "running"
        return "done" if done else "running"

    async def _publish_completion(self, run: PendingSandboxRun) -> None:
        """Announce the completion, and route the resume to the owner.

        Two publishes, two purposes. The ``crewlet.events.*`` copy is an
        ANNOUNCEMENT: the dashboard's broadcast stream watches that
        subject space, and dropping it would blank the Running-sandboxes
        panel. The per-seat control copy is a COMMAND, and it goes to the
        seat's own topic because only the node holding that seat has the
        suspended Execute conversation to resume — a single fleet-wide
        group would hand it to a non-owner (N-1)/N of the time.
        """
        completion = SandboxRunCompleted(
            source=run.role,
            agent_id=run.agent_id,
            agent_handle=run.agent_handle,
            role=run.role,
            turn_id=run.turn_id,
            sandbox_id=run.sandbox_id,
            coding_agent=run.coding_agent,
            # Carry the original trace so the completion turn nests.
            trace_id=run.trace_id,
            span_id=run.span_id,
        )
        await self._event_queue.publish(COMPLETIONS_TOPIC, completion)
        control = agent_control_topic(run.agent_handle)
        if control:
            await self._event_queue.publish(control, completion)


__all__ = ["SandboxWaiter"]
