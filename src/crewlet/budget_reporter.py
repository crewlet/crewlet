"""Publish the engine's live token meters so a dashboard can render them.

The counters in :class:`~crewlet.concurrency.BudgetManager` exist only in
the engine's memory, and nothing else can reconstruct them.  The two
figures a dashboard already holds cover different spans -- a seven-day
per-agent total and a twenty-four-hour spend rollup -- so neither can
stand in for a process-lifetime meter without being wrong by however long
the engine has been up.  Publishing the meter is the only honest way to
draw a seat's budget.

The reporter is a *coalescer*, not a stream.  ``BudgetManager`` fires its
change hook on the engine's hot path, under its own lock, once per LLM
round per agent; turning each of those into a published event would put
broker work inside the accounting critical section.  Instead the hook
sets a flag, and a tick publishes at most one report per interval.
"""

from __future__ import annotations

import asyncio
import contextlib
from collections.abc import Callable

from crewlet._logging import get_logger
from crewlet.concurrency import BudgetManager
from crewlet.events.types import BudgetReported
from crewlet.queue.protocol import EventQueue

logger = get_logger("budget.reporter")

# How often a moved meter is allowed to reach a screen.
#
# Matched to the API's own spend-rollup push interval
# (``_TOKENS_PUSH_INTERVAL_SECONDS`` in ``api/streaming.py``) and chosen
# for the same reason: the figure moves once per LLM round, which is
# seconds apart, and a reader's eye resolves about one update a second.
# Faster would publish several identical reports per round; slower would
# make a seat that just hit its cap look fine for longer than an operator
# would tolerate.
REPORT_INTERVAL_SECONDS = 1.0

TOPIC = "crewlet.events.budget_reported"


class BudgetReporter:
    """Coalesce meter changes into at most one report per tick."""

    def __init__(
        self,
        manager_of: Callable[[], BudgetManager | None],
        event_queue: EventQueue,
        role_of: Callable[[str], str],
        *,
        interval: float = REPORT_INTERVAL_SECONDS,
    ) -> None:
        # A callable rather than the manager itself: the engine replaces
        # ``budget_manager`` wholesale when the first configuration is
        # activated, and a captured reference would leave the reporter
        # publishing a meter nobody charges any more.
        self._manager_of = manager_of
        self._queue = event_queue
        self._role_of = role_of
        self._interval = interval
        self._dirty = False
        self._seq = 0
        self._task: asyncio.Task[None] | None = None

    # -- the hook -------------------------------------------------------

    def note_change(self, agent_id: str) -> None:
        """Mark the meter dirty.  Runs under the manager's lock."""
        self._dirty = True

    def attach(self) -> None:
        """Install the hook on the manager that is current *now*."""
        manager = self._manager_of()
        if manager is not None:
            manager.set_on_change(self.note_change)
            # A freshly attached manager has figures nobody has seen.
            self._dirty = True

    # -- lifecycle ------------------------------------------------------

    async def start(self) -> None:
        self.attach()
        if self._task is None or self._task.done():
            self._task = asyncio.create_task(self._loop())

    async def stop(self) -> None:
        task, self._task = self._task, None
        manager = self._manager_of()
        if manager is not None:
            manager.set_on_change(None)
        if task is None:
            return
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task

    async def _loop(self) -> None:
        while True:
            await asyncio.sleep(self._interval)
            if not self._dirty:
                continue
            try:
                await self.publish_once()
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # pragma: no cover - defensive
                # Telemetry must never take the engine down.
                logger.warning("budget_report_failed", error=str(exc))

    # -- the report -----------------------------------------------------

    async def publish_once(self) -> BudgetReported | None:
        """Publish one report of the current meters, or ``None``."""
        manager = self._manager_of()
        if manager is None:
            return None
        # Cleared BEFORE reading, so a charge landing during the read is
        # counted by the next tick rather than being swallowed by this
        # one. The cost of the race in this direction is one redundant
        # report; in the other it is a figure frozen until something
        # else moves.
        self._dirty = False
        snapshot = manager.report()
        self._seq += 1
        # Only seats the engine actually meters appear here: it seeds a
        # per-agent budget solely for a non-zero ``Role.token_budget``,
        # so absence means "this seat has no cap and no meter", which is
        # a different fact from a cap of zero and must not be rendered
        # as an empty bar.
        agents = [
            {
                "agent_id": agent_id,
                "role": self._role_of(agent_id),
                "used_tokens": figures["used_tokens"],
                "max_tokens": figures["max_tokens"],
                "refused_at": figures["refused_at"],
            }
            for agent_id, figures in sorted(snapshot["agents"].items())
        ]
        event = BudgetReported(
            source="budget",
            meter_id=str(snapshot["meter_id"]),
            seq=self._seq,
            org_used_tokens=int(snapshot["org_used_tokens"]),
            org_max_tokens=int(snapshot["org_max_tokens"]),
            org_refused_at=str(snapshot["org_refused_at"]),
            agents=agents,
        )
        await self._queue.publish(TOPIC, event)
        return event
