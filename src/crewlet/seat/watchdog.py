"""The one thing a stalled process can still do about itself: leave.

The obvious design is a lease heartbeat on a dedicated OS thread that
"kills its own seat work" when the event loop stalls. It cannot: a
stalled loop does not process ``call_soon_threadsafe`` either, so no
cross-thread signal can stop the work. Anything the thread schedules
waits behind the same blockage it is reacting to.

What the thread *can* do unilaterally is end the process.

That matters because of the shape of the failure. A node whose loop is
wedged but whose process is alive keeps its broker session open — the
Pulsar client's IO threads answer keepalives from their own threads — so
the broker goes on treating it as a live consumer and holding its
prefetch for it. Measured against a real broker, that lasts the full
unacked-message timeout: **~30 minutes** at the engine's configured
value. Meanwhile the seat leases lapse (nothing is renewing them), a peer
takes the seats over, and the wedged node still holds a queue of their
messages. Exiting collapses that: the client dies with the process, the
broker sees the session end, and the measurements say redelivery is
immediate — 9 ms.

So the heartbeat itself stays an asyncio task, whose failure mode is
already the safe one (a stalled loop stops renewing, leases lapse, peers
take over). This watchdog exists purely to convert "wedged and holding
things hostage" into "gone", and it is deliberately the crudest possible
mechanism: ``os._exit``, no unwinding, no handlers. A process that cannot
run its event loop cannot be trusted to run a graceful shutdown, and
trying is how a watchdog ends up wedged too.
"""

from __future__ import annotations

import asyncio
import contextlib
import os
import sys
import threading
import time
from collections.abc import Callable
from typing import Any

from crewlet._logging import get_logger

logger = get_logger("seat.watchdog")

# How often the in-loop beat updates, and how often the thread checks.
#
# One second each. The check is a monotonic subtraction and a comparison,
# so its cost is irrelevant next to the resolution it buys.
#
# These are CEILINGS, not fixed values: both are scaled down against the
# threshold in the constructor. The invariant is that a healthy loop must
# never look stalled, and a beat that is slow relative to the threshold
# breaks it — at a 0.5 s threshold and a 1 s beat, a perfectly healthy
# process reports itself a full second behind and shoots itself. That is
# invisible at production values (45 s vs 1 s) and lethal to anyone who
# lowers the lease TTL, so it is enforced rather than documented.
WATCHDOG_BEAT_SECONDS = 1.0
WATCHDOG_POLL_SECONDS = 1.0

# How many beats must fit inside the threshold. Five gives a healthy loop
# four chances to refresh before the window closes, so ordinary jitter
# never registers.
WATCHDOG_BEATS_PER_THRESHOLD = 5

# Exit code for a self-terminated wedge, distinct from any ordinary
# failure so an orchestrator's restart logs say what happened.
WATCHDOG_EXIT_CODE = 75


class EventLoopWatchdog:
    """Hard-exits the process when the event loop stalls past ``threshold``.

    ``threshold`` should be the seat lease TTL: past it the node's leases
    have lapsed, peers may already have taken its seats over, and every
    further second it stays alive is a second it holds their messages in
    a prefetch buffer nobody can reach.

    The default threshold is deliberately *not* wired to a config knob.
    It is not a tuning parameter — it is the same number the lease TTL
    is, and letting the two drift is how a node gets to be simultaneously
    "not the owner" and "still holding the mail".
    """

    __slots__ = (
        "_beat",
        "_beat_interval",
        "_beat_task",
        "_loop",
        "_on_stall",
        "_poll_interval",
        "_started",
        "_stop",
        "_thread",
        "_threshold",
    )

    def __init__(
        self,
        *,
        threshold_seconds: float,
        on_stall: Callable[[float], None] | None = None,
    ) -> None:
        self._threshold = float(threshold_seconds)
        # Scale the beat and the poll to the threshold — see the
        # constants above for the invariant this protects.
        scaled = self._threshold / WATCHDOG_BEATS_PER_THRESHOLD
        self._beat_interval = min(WATCHDOG_BEAT_SECONDS, scaled)
        self._poll_interval = min(WATCHDOG_POLL_SECONDS, scaled)
        # Seeded now rather than at 0.0: an unstarted watchdog must not
        # read as "stalled since the epoch".
        self._beat = time.monotonic()
        self._beat_task: asyncio.Task[Any] | None = None
        # The loop the beat runs on, captured at ``start``. A watchdog
        # that does not know this cannot tell a WEDGED loop from a GONE
        # one, and they look identical from the thread: in both cases the
        # beat simply stops refreshing. See ``_watch``.
        self._loop: asyncio.AbstractEventLoop | None = None
        self._thread: threading.Thread | None = None
        self._stop = threading.Event()
        self._started = False
        # Injection seam for tests — the default really does exit.
        self._on_stall = on_stall if on_stall is not None else _hard_exit

    @property
    def lag_seconds(self) -> float:
        """How long since the event loop last ran the beat."""
        return time.monotonic() - self._beat

    def start(self) -> None:
        """Begin beating (in the loop) and watching (in a thread)."""
        if self._started:
            return
        self._started = True
        self._beat = time.monotonic()
        self._stop.clear()
        self._loop = asyncio.get_running_loop()
        self._beat_task = asyncio.create_task(self._beat_loop())
        self._thread = threading.Thread(
            target=self._watch,
            name="crewlet-loop-watchdog",
            # Daemon: this thread must never be the reason a healthy
            # process refuses to exit.
            daemon=True,
        )
        self._thread.start()
        logger.info("loop_watchdog_started", threshold_seconds=self._threshold)

    async def stop(self) -> None:
        if not self._started:
            return
        self._started = False
        self._stop.set()
        if self._beat_task is not None:
            self._beat_task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._beat_task
            self._beat_task = None
        thread, self._thread = self._thread, None
        if thread is not None:
            thread.join(timeout=self._poll_interval * 2 + 0.5)
        self._loop = None
        logger.info("loop_watchdog_stopped")

    async def _beat_loop(self) -> None:
        while True:
            self._beat = time.monotonic()
            await asyncio.sleep(self._beat_interval)

    def _watch(self) -> None:
        while not self._stop.wait(self._poll_interval):
            lag = self.lag_seconds
            if lag <= self._threshold:
                continue
            if not self._loop_is_wedged():
                # The beat stopped because its LOOP went away, not
                # because the loop stopped turning — an engine that was
                # abandoned rather than stopped, a `run_until_complete`
                # that returned, a test's loop torn down underneath it.
                #
                # Both look identical from here: the beat simply stops
                # refreshing. But there is nothing to shoot. The whole
                # justification for exiting is that a wedged-BUT-ALIVE
                # loop keeps the broker session open and holds a peer's
                # messages hostage; a loop that is closed took the client
                # and its IO threads with it. Killing the process here
                # would be a 45-second suicide timer armed by any engine
                # that was not explicitly stopped.
                logger.info("loop_watchdog_stood_down", lag_seconds=round(lag, 1))
                return
            self._on_stall(lag)
            return

    def _loop_is_wedged(self) -> bool:
        """Whether the stalled beat means a live loop that stopped turning.

        Read from the watchdog thread, so it touches only what is safe to
        read across threads: ``is_closed`` and ``is_running`` are plain
        attribute reads, not loop operations.
        """
        loop = self._loop
        if loop is None:
            return False
        return not loop.is_closed() and loop.is_running()


def _hard_exit(lag: float) -> None:
    """Leave, now, without unwinding.

    ``os._exit`` rather than ``sys.exit`` or a signal: the event loop is
    wedged, so anything that needs the loop — async context managers,
    ``atexit`` handlers that touch it, a graceful drain — will hang. The
    log line is written to stderr directly for the same reason: a
    structlog processor chain is more machinery than a wedged process has
    earned.
    """
    print(
        f"crewlet: event loop stalled {lag:.1f}s past the seat lease TTL; "
        f"exiting so peers can take over cleanly (code {WATCHDOG_EXIT_CODE})",
        file=sys.stderr,
        flush=True,
    )
    os._exit(WATCHDOG_EXIT_CODE)
