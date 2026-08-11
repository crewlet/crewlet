"""Scheduler — role/unit-scoped cron-style task dispatch.

A single asyncio loop ticks on a short interval (default 10s).  Each
tick enumerates every ``Schedule`` across the live org (roles **and**
units), works out which schedules are due since the previous tick, and
for each due fire resolves the runner agent(s) and publishes a
``TaskAssigned`` to their inbox topic — reusing the existing agent
runtime path unchanged.

Delivery (unit schedules):

* ``each`` (default) — one task per **direct** member of the unit; each
  member runs independently with its own idempotency key, so a slow or
  failing member never blocks the others.
* ``lead`` — a single task to the unit's effective lead.
* ``<role>`` — a single task to a named role/handle in the unit.

Guarantees:

* **At-most-once** across restarts and re-ticks via a unique idempotency
  key claimed in the store before publishing.
* **Missed-tick catchup** is evaluated only on the first tick after
  (re)start: the single most-recent missed fire is fired if it falls
  inside the catchup window (half the schedule's period, clamped to
  ``[catchup_min, catchup_max]``); older misses are never backfilled.
* **Hot reload** is free — each tick reads the current org via
  ``org_provider`` (the engine swaps ``self.org`` in ``reload_config``),
  so added/removed schedules start/stop on the next tick.

The per-run **3-minute hard timeout** is enforced one layer down, in the
turn engine, via the ``timeout_seconds`` carried on the published task's
payload (see ``Engine._make_agent_handler``).
"""

from __future__ import annotations

import asyncio
import contextlib
import hashlib
from collections.abc import Callable, Iterator
from datetime import UTC, datetime, timedelta
from typing import Any
from zoneinfo import ZoneInfo

from opentelemetry.context import Context as OtelContext

from crewlet._logging import get_logger
from crewlet.events.types import ScheduledTaskFired, TaskAssigned
from crewlet.org.hierarchy import get_effective_lead
from crewlet.org.models import (
    Organization,
    OrgUnit,
    RoleKind,
    Schedule,
    ScheduleTarget,
)
from crewlet.schedule.cron import (
    CronExpr,
    iter_fire_times,
    next_fire,
    parse_cron,
    prev_fire,
)
from crewlet.schedule.store import ScheduledRunStoreProtocol
from crewlet.telemetry import current_trace_id, set_span_error, tracer

logger = get_logger("schedule.scheduler")


def has_schedules(org: Any) -> bool:
    """True if any role or unit in *org* defines at least one schedule."""
    try:
        if any(r.schedules for r in org.all_roles()):
            return True
        return any(u.schedules for u in org.all_units())
    except Exception:
        return False


def count_schedules(org: Any) -> int:
    """Total number of schedules across roles and units."""
    total = 0
    try:
        for r in org.all_roles():
            total += len(r.schedules)
        for u in org.all_units():
            total += len(u.schedules)
    except Exception:
        return total
    return total


class Scheduler:
    """Periodic cron-style dispatcher for role/unit schedules."""

    def __init__(
        self,
        *,
        event_queue: Any,
        agent_pool: Any,
        org_provider: Callable[[], Organization],
        store: ScheduledRunStoreProtocol,
        default_timezone: str = "UTC",
        tick_seconds: int = 10,
        jitter_seconds: int = 0,
        catchup_min_seconds: int = 120,
        catchup_max_seconds: int = 7200,
    ) -> None:
        self._event_queue = event_queue
        self._agent_pool = agent_pool
        self._org_provider = org_provider
        self._store = store
        self._default_tz = default_timezone or "UTC"
        self._tick_seconds = max(1, int(tick_seconds))
        self._jitter_seconds = max(0, int(jitter_seconds))
        self._catchup_min = max(0, int(catchup_min_seconds))
        self._catchup_max = max(self._catchup_min, int(catchup_max_seconds))
        self._running = False
        self._task: asyncio.Task[None] | None = None
        # UTC time of the previous tick; ``None`` until the first tick,
        # which triggers missed-tick catchup evaluation.
        self._last_tick_utc: datetime | None = None

    # ----- lifecycle --------------------------------------------------

    async def start(self) -> None:
        if self._running:
            return
        self._running = True
        loop = asyncio.get_event_loop()
        self._task = loop.create_task(self._run_loop(), name="scheduler")
        logger.info(
            "scheduler_started",
            tick_seconds=self._tick_seconds,
            default_timezone=self._default_tz,
            jitter_seconds=self._jitter_seconds,
        )

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
        logger.info("scheduler_stopped")

    async def _run_loop(self) -> None:
        try:
            while self._running:
                try:
                    await asyncio.sleep(self._tick_seconds)
                except asyncio.CancelledError:
                    return
                if not self._running:
                    return
                try:
                    await self.tick_once()
                except asyncio.CancelledError:
                    return
                except Exception:
                    logger.exception("scheduler_tick_failed")
        finally:
            self._running = False

    # ----- tick -------------------------------------------------------

    async def tick_once(self, now: datetime | None = None) -> int:
        """Evaluate every schedule once and dispatch due fires.

        Returns the number of ``TaskAssigned`` events published.  Exposed
        so tests can drive the scheduler without waiting on the interval.
        """
        if now is None:
            now = datetime.now(UTC)
        elif now.tzinfo is None:
            now = now.replace(tzinfo=UTC)

        first = self._last_tick_utc is None
        window_start = self._last_tick_utc
        org = self._org_provider()

        fired = 0
        with tracer.start_as_current_span(
            "schedule.scheduler.tick",
            attributes={"schedule.first_tick": first},
        ) as span:
            for scope_type, scope_id, unit, schedule in _iter_schedules(org):
                if not schedule.enabled:
                    continue
                try:
                    tz = ZoneInfo(schedule.timezone or self._default_tz)
                    cron = parse_cron(schedule.cron)
                except Exception as exc:
                    set_span_error(exc)
                    logger.exception(
                        "schedule_parse_failed",
                        schedule=schedule.name,
                        scope_type=scope_type,
                        scope_id=scope_id,
                    )
                    continue

                to_fire, to_skip = self._due_fire_times(
                    cron=cron,
                    tz=tz,
                    schedule=schedule,
                    scope_id=scope_id,
                    now=now,
                    window_start=window_start,
                    first=first,
                )
                for ft in to_skip:
                    await self._record_skip(scope_type, scope_id, schedule, ft, tz)
                for ft in to_fire:
                    runners = self._resolve_runners(
                        org, scope_type, scope_id, unit, schedule
                    )
                    if not runners:
                        logger.warning(
                            "schedule_no_runners",
                            schedule=schedule.name,
                            scope_type=scope_type,
                            scope_id=scope_id,
                            target=schedule.target,
                        )
                        continue
                    for handle in runners:
                        if await self._fire(
                            scope_type, scope_id, schedule, handle, ft, tz
                        ):
                            fired += 1
            span.set_attribute("schedule.fired", fired)

        self._last_tick_utc = now
        return fired

    # ----- due-time computation --------------------------------------

    def _due_fire_times(
        self,
        *,
        cron: CronExpr,
        tz: ZoneInfo,
        schedule: Schedule,
        scope_id: str,
        now: datetime,
        window_start: datetime | None,
        first: bool,
    ) -> tuple[list[datetime], list[datetime]]:
        """Return ``(to_fire, to_skip)`` canonical UTC fire times.

        A per-schedule jitter shifts each schedule's *effective* firing
        instant by a deterministic offset so common cron minutes (e.g.
        ``0 9 * * *``) don't stampede; the canonical (un-shifted) fire
        time is what forms the idempotency key, so dedup is unaffected.
        """
        jitter = timedelta(seconds=self._jitter_for(scope_id, schedule.name))
        eff_now = now - jitter

        if not first:
            if window_start is None:
                return [], []
            fires = iter_fire_times(
                cron,
                after_utc=window_start - jitter,
                until_utc=eff_now,
                tz=tz,
            )
            return fires, []

        # First tick after (re)start — evaluate a single catchup fire.
        prev = prev_fire(cron, at_utc=eff_now, tz=tz)
        if prev is None or not schedule.catchup:
            return [], []
        window = self._catchup_window(cron, tz, eff_now)
        age = (eff_now - prev).total_seconds()
        if age <= window:
            return [prev], []
        return [], [prev]

    def _catchup_window(self, cron: CronExpr, tz: ZoneInfo, ref_utc: datetime) -> float:
        """Catchup window in seconds: half the period, clamped to bounds."""
        t1 = next_fire(cron, after_utc=ref_utc, tz=tz)
        if t1 is None:
            return float(self._catchup_min)
        t2 = next_fire(cron, after_utc=t1, tz=tz)
        if t2 is None:
            return float(self._catchup_min)
        period = (t2 - t1).total_seconds()
        return max(float(self._catchup_min), min(float(self._catchup_max), period / 2))

    def _jitter_for(self, scope_id: str, name: str) -> int:
        if self._jitter_seconds <= 0:
            return 0
        digest = hashlib.sha256(f"{scope_id}:{name}".encode()).digest()
        return int.from_bytes(digest[:8], "big") % (self._jitter_seconds + 1)

    # ----- runner resolution -----------------------------------------

    def _resolve_runners(
        self,
        org: Organization,
        scope_type: str,
        scope_id: str,
        unit: OrgUnit | None,
        schedule: Schedule,
    ) -> list[str]:
        """Resolve the handle(s) that should run this schedule."""
        return resolve_runners(org, scope_type, scope_id, unit, schedule)

    # ----- firing -----------------------------------------------------

    async def _fire(
        self,
        scope_type: str,
        scope_id: str,
        schedule: Schedule,
        handle: str,
        fire_time_utc: datetime,
        tz: ZoneInfo,
    ) -> bool:
        agent = self._agent_pool.get_by_handle(handle)
        if agent is None:
            # No live agent to run it — don't claim, so a later tick after
            # the agent spawns could still pick up a future fire.
            logger.warning(
                "schedule_runner_not_found",
                handle=handle,
                schedule=schedule.name,
                scope_id=scope_id,
            )
            return False

        fire_label = _fire_label(fire_time_utc, tz)
        # Each dispatched run gets its OWN trace (a fresh root span, detached
        # from the tick) so the dashboard can link the ledger row to exactly
        # this turn's Plan/Execute/Review calls.  The TaskAssigned created
        # inside the span carries the new trace_id; the agent's turn restores
        # it, so the whole turn shares this trace.
        with tracer.start_as_current_span(
            "schedule.fire",
            context=OtelContext(),
            attributes={
                "schedule.name": schedule.name,
                "schedule.scope_type": scope_type,
                "schedule.scope_id": scope_id,
                "schedule.runner": handle,
            },
        ):
            trace_id = current_trace_id()
            try:
                claimed = await self._store.claim(
                    scope_type=scope_type,
                    scope_id=scope_id,
                    schedule_name=schedule.name,
                    fire_label=fire_label,
                    target_handle=handle,
                    scheduled_at=fire_time_utc,
                    outcome="fired",
                    trace_id=trace_id,
                )
            except Exception:
                logger.exception(
                    "schedule_claim_failed", schedule=schedule.name, handle=handle
                )
                return False
            if not claimed:
                return False

            run_id = _run_id(scope_type, scope_id, schedule.name, fire_label, handle)
            event = TaskAssigned(
                source="scheduler",
                task_id=run_id,
                agent_id=agent.id_str,
                role=agent.role_name,
                payload={
                    "task_description": schedule.task,
                    "scheduled": True,
                    "schedule_name": schedule.name,
                    "scope_type": scope_type,
                    "scope_id": scope_id,
                    "timeout_seconds": schedule.timeout_seconds,
                    "run_id": run_id,
                },
            )
            try:
                await self._event_queue.publish(
                    f"crewlet.agent.{agent.handle}.inbox", event
                )
            except Exception:
                logger.exception(
                    "schedule_publish_failed", schedule=schedule.name, handle=handle
                )
                return False

            logger.info(
                "schedule_fired",
                schedule=schedule.name,
                scope_type=scope_type,
                scope_id=scope_id,
                handle=handle,
                scheduled_at=fire_time_utc.isoformat(),
                trace_id=trace_id,
            )
            await self._publish_lifecycle(
                scope_type, scope_id, schedule, handle, fire_time_utc
            )
            return True

    async def _record_skip(
        self,
        scope_type: str,
        scope_id: str,
        schedule: Schedule,
        fire_time_utc: datetime,
        tz: ZoneInfo,
    ) -> None:
        """Record (once) that a missed catchup fire was deliberately skipped.

        The skip row has ``target_handle=''`` (no runner resolved yet), so
        it never collides with a fire row for the same minute (every fire
        carries a non-empty runner handle).
        """
        try:
            await self._store.claim(
                scope_type=scope_type,
                scope_id=scope_id,
                schedule_name=schedule.name,
                fire_label=_fire_label(fire_time_utc, tz),
                target_handle="",
                scheduled_at=fire_time_utc,
                outcome="skipped_catchup",
            )
        except Exception:
            logger.exception("schedule_skip_record_failed", schedule=schedule.name)
        logger.info(
            "schedule_catchup_skipped",
            schedule=schedule.name,
            scope_type=scope_type,
            scope_id=scope_id,
            scheduled_at=fire_time_utc.isoformat(),
        )

    async def _publish_lifecycle(
        self,
        scope_type: str,
        scope_id: str,
        schedule: Schedule,
        handle: str,
        fire_time_utc: datetime,
    ) -> None:
        """Best-effort observability event for the dashboard/event store."""
        try:
            await self._event_queue.publish(
                "crewlet.events.scheduled_task_fired",
                ScheduledTaskFired(
                    source="scheduler",
                    scope_type=scope_type,
                    scope_id=scope_id,
                    schedule_name=schedule.name,
                    target_handle=handle,
                    scheduled_at=fire_time_utc.isoformat(),
                ),
            )
        except Exception:
            logger.exception(
                "scheduled_task_fired_publish_failed", schedule=schedule.name
            )


def _fire_label(fire_time_utc: datetime, tz: ZoneInfo) -> str:
    """Local wall-clock stamp identifying a fire within its schedule.

    Local (not UTC) so dedup is DST-correct: on a fall-back day the
    repeated local minute shares one label and fires once, rather than
    twice (its two UTC instants).
    """
    return fire_time_utc.astimezone(tz).strftime("%Y%m%dT%H%M")


def _run_id(
    scope_type: str,
    scope_id: str,
    schedule_name: str,
    fire_label: str,
    runner: str,
) -> str:
    """Human-readable id for a dispatched task (telemetry only).

    The durable at-most-once guarantee is the composite key in
    ``scheduled_runs``, so a collision in this readable id (e.g. from a
    ``:`` in a name) is harmless — it never gates a dispatch.
    """
    return f"{scope_type}:{scope_id}:{schedule_name}:{fire_label}:{runner}"


def _iter_schedules(
    org: Any,
) -> Iterator[tuple[str, str, OrgUnit | None, Schedule]]:
    """Yield ``(scope_type, scope_id, unit_or_None, schedule)`` for the org."""
    for role in org.all_roles():
        for sch in role.schedules:
            yield ("role", role.get_handle(), None, sch)
    for unit in org.all_units():
        for sch in unit.schedules:
            yield ("unit", unit.name, unit, sch)


def resolve_runners(
    org: Any,
    scope_type: str,
    scope_id: str,
    unit: OrgUnit | None,
    schedule: Schedule,
) -> list[str]:
    """Resolve the handle(s) that should run a schedule.

    Module-level so the Scheduler and the dashboard's read-only
    :func:`describe_schedules` projection share one resolution path.
    ``each`` resolves the unit's **direct** agent roles only (never
    descendants, never human seats — humans run no turns); ``lead``
    resolves the unit's effective lead, or nobody when the effective
    lead is a human seat.  Config validation rejects both human
    combinations up front; the runtime filter covers the lenient
    piecewise-bootstrap window, where the tick's existing
    ``schedule_no_runners`` warning surfaces the gap.
    """
    if scope_type == "role":
        # scope_id is the role's handle.  Human seats can't define
        # schedules (model-validated), so this is always an agent.
        return [scope_id]
    if unit is None:
        return []
    if schedule.target == ScheduleTarget.LEAD.value:
        lead = get_effective_lead(unit, org)
        if lead is None or lead.kind == RoleKind.HUMAN:
            return []
        return [lead.get_handle()]
    # ``each`` (the default / only other valid target).
    return [r.get_handle() for r in unit.roles if r.kind == RoleKind.AGENT]


def describe_schedules(
    org: Any, *, default_timezone: str = "UTC"
) -> list[dict[str, Any]]:
    """Project every schedule in *org* into a JSON-friendly dict.

    Used by the dashboard's ``/schedules`` endpoint. Resolves the
    effective timezone and runner handles statically; the time-dependent
    ``next_run`` and the run history are added by the API layer.
    """
    out: list[dict[str, Any]] = []
    for scope_type, scope_id, unit, sch in _iter_schedules(org):
        out.append(
            {
                "scope_type": scope_type,
                "scope_id": scope_id,
                "name": sch.name,
                "cron": sch.cron,
                "timezone": sch.timezone or default_timezone,
                "task": sch.task,
                "target": sch.target if scope_type == "unit" else "",
                "enabled": sch.enabled,
                "timeout_seconds": sch.timeout_seconds,
                "catchup": sch.catchup,
                "runners": resolve_runners(org, scope_type, scope_id, unit, sch),
            }
        )
    return out


__all__ = [
    "Scheduler",
    "count_schedules",
    "describe_schedules",
    "has_schedules",
    "resolve_runners",
]
