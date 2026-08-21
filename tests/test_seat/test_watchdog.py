"""The watchdog: what a wedged process can still do about itself.

The design review established that a dedicated thread *cannot* stop the
work of a stalled event loop — a stalled loop does not run
``call_soon_threadsafe`` either, so anything the thread schedules waits
behind the very blockage it is reacting to. What it can do is end the
process, and that is worth doing for a measured reason: a wedged-but-alive
node keeps its broker session open (the Pulsar client answers keepalives
from its own IO threads), so the broker holds its prefetch for it for a
full unacked-message timeout — ~30 minutes at the engine's setting. Its
seat leases meanwhile lapse and peers take the seats over. Exiting
collapses that window to the 9 ms a closed session takes to release.

These tests use the ``on_stall`` seam rather than letting the default
fire, for the obvious reason.
"""

from __future__ import annotations

import asyncio
import time

from crewlet.seat.watchdog import (
    WATCHDOG_BEAT_SECONDS,
    WATCHDOG_BEATS_PER_THRESHOLD,
    WATCHDOG_EXIT_CODE,
    WATCHDOG_POLL_SECONDS,
    EventLoopWatchdog,
)


async def test_a_healthy_loop_is_never_killed() -> None:
    """The failure that would matter most: a watchdog that fires on a
    perfectly healthy process."""
    fired: list[float] = []
    dog = EventLoopWatchdog(threshold_seconds=0.5, on_stall=fired.append)
    dog.start()
    try:
        # Yield often, as a healthy loop does.
        for _ in range(30):
            await asyncio.sleep(0.05)
    finally:
        await dog.stop()
    assert fired == []


async def test_a_blocked_loop_trips_the_watchdog() -> None:
    """Block the loop the way a real wedge does — synchronously, so the
    beat coroutine cannot run — and confirm the thread notices without
    needing the loop's cooperation."""
    fired: list[float] = []
    dog = EventLoopWatchdog(threshold_seconds=0.3, on_stall=fired.append)
    dog.start()
    try:
        await asyncio.sleep(0.05)
        # A synchronous sleep IS the stall: no await, so nothing else on
        # this loop runs, including the beat.
        time.sleep(0.3 + dog._poll_interval + 0.4)
    finally:
        await dog.stop()

    assert fired, "the watchdog slept through a fully blocked event loop"
    assert fired[0] > 0.3


async def test_lag_reports_how_far_behind_the_loop_is() -> None:
    dog = EventLoopWatchdog(threshold_seconds=10.0, on_stall=lambda lag: None)
    dog.start()
    try:
        await asyncio.sleep(0.05)
        assert dog.lag_seconds < 1.0
        time.sleep(0.4)
        assert dog.lag_seconds >= 0.4
    finally:
        await dog.stop()


async def test_an_unstarted_watchdog_does_not_read_as_stalled() -> None:
    """Seeded at construction, not at zero — otherwise a watchdog would
    report itself stalled since the epoch before it ever ran."""
    dog = EventLoopWatchdog(threshold_seconds=1.0)
    assert dog.lag_seconds < 1.0


async def test_stop_is_idempotent_and_ends_the_thread() -> None:
    dog = EventLoopWatchdog(threshold_seconds=5.0, on_stall=lambda lag: None)
    dog.start()
    dog.start()  # second start is a no-op, not a second thread
    await dog.stop()
    await dog.stop()
    assert dog._thread is None


async def test_the_watchdog_fires_only_once() -> None:
    """It exits the process, so 'again' is meaningless — but the loop
    must not spin re-firing while a test seam keeps the process alive."""
    fired: list[float] = []
    dog = EventLoopWatchdog(threshold_seconds=0.2, on_stall=fired.append)
    dog.start()
    try:
        await asyncio.sleep(0.05)
        time.sleep(0.2 + dog._poll_interval * 4)
    finally:
        await dog.stop()
    assert len(fired) == 1


def test_the_exit_code_is_distinct() -> None:
    """So an orchestrator's restart logs say what happened rather than
    blending into every other non-zero exit."""
    assert WATCHDOG_EXIT_CODE not in (0, 1, 2)


async def test_the_beat_is_scaled_to_the_threshold() -> None:
    """The invariant a healthy loop's life depends on.

    A beat slower than the threshold makes a perfectly healthy process
    report itself stalled. Production values hide it (45 s threshold, 1 s
    beat); anyone lowering the lease TTL would find it the hard way.
    """
    tight = EventLoopWatchdog(threshold_seconds=0.5, on_stall=lambda lag: None)
    assert tight._beat_interval <= 0.5 / WATCHDOG_BEATS_PER_THRESHOLD
    assert tight._poll_interval <= 0.5 / WATCHDOG_BEATS_PER_THRESHOLD

    # At production scale the ceilings apply instead — no point beating
    # nine times a second because the TTL is generous.
    roomy = EventLoopWatchdog(threshold_seconds=45.0, on_stall=lambda lag: None)
    assert roomy._beat_interval == WATCHDOG_BEAT_SECONDS
    assert roomy._poll_interval == WATCHDOG_POLL_SECONDS


async def test_a_healthy_loop_survives_a_very_tight_threshold() -> None:
    """The regression guard for the above, end to end."""
    fired: list[float] = []
    dog = EventLoopWatchdog(threshold_seconds=0.4, on_stall=fired.append)
    dog.start()
    try:
        for _ in range(40):
            await asyncio.sleep(0.02)
    finally:
        await dog.stop()
    assert fired == []


# ── the wiring ──────────────────────────────────────────────────────


class TestTheEngineActuallyArmsIt:
    """The failure this class exists to prevent is not a bug in the
    watchdog — it is a watchdog that is never started.

    Everything above tested a `EventLoopWatchdog` the tests constructed
    themselves. The class shipped fully implemented, exported and
    covered, and nothing in the engine ever built one, so the remedy the
    seat design leans on ("undead seats are not held forever") did not
    exist at runtime. These pin the wiring instead of the mechanism.
    """

    @staticmethod
    async def _engine():
        import sys
        from pathlib import Path

        sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
        from conftest import make_company, make_engine
        from crewlet.queue.memory import MemoryEventQueue

        return await make_engine(company=make_company(), event_queue=MemoryEventQueue())

    async def test_starting_the_engine_arms_the_watchdog(self) -> None:
        engine = await self._engine()
        await engine.start()
        try:
            assert engine._loop_watchdog is not None
            assert engine._loop_watchdog.lag_seconds < 5.0
        finally:
            await engine.stop()

    async def test_its_threshold_is_the_lease_ttl(self) -> None:
        """Not a config knob, and not a number of its own. Past the TTL
        the node is provably not the owner; letting the two drift is how
        a process gets to be "not the owner" and "still holding the
        mail" at the same time."""
        from crewlet.seat.host import SEAT_LEASE_TTL_SECONDS

        engine = await self._engine()
        await engine.start()
        try:
            assert engine._loop_watchdog._threshold == SEAT_LEASE_TTL_SECONDS
        finally:
            await engine.stop()

    async def test_stopping_the_engine_disarms_it(self) -> None:
        """Teardown is the one part of the process that legitimately
        blocks the loop — reaping MCP subprocesses, joining threads — so
        a watchdog left armed through it would `os._exit` in the middle
        of the seat release that makes a drain graceful."""
        engine = await self._engine()
        await engine.start()
        watchdog = engine._loop_watchdog
        await engine.stop()
        assert engine._loop_watchdog is None
        assert watchdog._started is False

    async def test_force_stop_disarms_it_too(self) -> None:
        engine = await self._engine()
        await engine.start()
        assert engine._loop_watchdog is not None
        await engine._force_stop()
        assert engine._loop_watchdog is None


# ── a gone loop is not a wedged loop ────────────────────────────────


def test_a_closed_loop_stands_down_instead_of_exiting() -> None:
    """The distinction the whole mechanism turns on.

    From the watching thread, "the loop is wedged" and "the loop is
    gone" look identical: the beat simply stops refreshing. They are
    opposite situations. A wedged-BUT-ALIVE loop keeps its broker
    session open and holds a peer's messages hostage, which is the
    entire justification for exiting. A loop that closed took the client
    and its IO threads with it, so there is nothing left to hold.

    Getting this wrong is not a subtle bug: every engine that is
    abandoned rather than stopped — an embedding that drops its
    reference, a `run_until_complete` that returned, a test's loop torn
    down underneath it — arms a suicide timer that fires one lease TTL
    later on a perfectly healthy process. It fired on this repo's own
    test suite, killing the run at 63% with exit code 75.
    """
    fired: list[float] = []
    dog = EventLoopWatchdog(threshold_seconds=0.2, on_stall=fired.append)

    loop = asyncio.new_event_loop()
    try:
        loop.run_until_complete(_arm(dog))
    finally:
        _abandon(dog, loop)

    # The beat can never refresh again — its loop is gone.
    time.sleep(0.6)
    assert fired == [], "a closed loop was mistaken for a wedged one"
    # And it stops watching rather than spinning for the life of the
    # process: the thread returns on the first poll past the threshold.
    dog._thread.join(timeout=2.0)
    assert not dog._thread.is_alive()


def test_a_stopped_but_open_loop_stands_down_too() -> None:
    """`run_until_complete` returning leaves the loop open but not
    running. Nothing is scheduling, so nothing is wedged."""
    fired: list[float] = []
    dog = EventLoopWatchdog(threshold_seconds=0.2, on_stall=fired.append)
    loop = asyncio.new_event_loop()
    try:
        loop.run_until_complete(_arm(dog))
        time.sleep(0.6)
        assert fired == []
    finally:
        _abandon(dog, loop)


async def _arm(dog: EventLoopWatchdog) -> None:
    dog.start()
    await asyncio.sleep(0)


def _abandon(dog: EventLoopWatchdog, loop: asyncio.AbstractEventLoop) -> None:
    """Drop the loop while leaving the watching thread armed.

    The beat task is reaped first only to keep "Task was destroyed but it
    is pending" out of the test output. It changes nothing under test:
    the thread cannot see whether the beat was cancelled or garbage —
    only that it stopped refreshing, which is the whole point.
    """
    if dog._beat_task is not None:
        dog._beat_task.cancel()
        loop.run_until_complete(asyncio.sleep(0))
    loop.close()
