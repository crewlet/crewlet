"""Behavioural tests for the Scheduler dispatch loop."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from typing import Any

from crewlet.events.types import ScheduledTaskFired, TaskAssigned
from crewlet.org.models import Organization, OrgUnit, Role, Schedule
from crewlet.schedule import MemoryScheduledRunStore, Scheduler
from crewlet.schedule.scheduler import count_schedules, has_schedules

# --- fakes -----------------------------------------------------------------


@dataclass
class _Agent:
    handle: str
    role_name: str
    id_str: str


class _Pool:
    def __init__(self, agents: list[_Agent]) -> None:
        self._by_handle = {a.handle: a for a in agents}

    def get_by_handle(self, handle: str) -> _Agent | None:
        return self._by_handle.get(handle)


@dataclass
class _Queue:
    published: list[tuple[str, Any]] = field(default_factory=list)

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))

    def inbox_tasks(self) -> list[tuple[str, TaskAssigned]]:
        return [
            (t, e)
            for t, e in self.published
            if isinstance(e, TaskAssigned) and t.endswith(".inbox")
        ]


def _agents_for(org: Organization) -> list[_Agent]:
    return [
        _Agent(handle=r.get_handle(), role_name=r.name, id_str=f"id-{r.get_handle()}")
        for r in org.all_roles()
    ]


def _build(
    org: Organization,
    *,
    jitter_seconds: int = 0,
) -> tuple[Scheduler, _Queue, MemoryScheduledRunStore, list[Organization]]:
    holder = [org]
    queue = _Queue()
    store = MemoryScheduledRunStore()
    scheduler = Scheduler(
        event_queue=queue,
        agent_pool=_Pool(_agents_for(org)),
        org_provider=lambda: holder[0],
        store=store,
        default_timezone="UTC",
        tick_seconds=10,
        jitter_seconds=jitter_seconds,
    )
    return scheduler, queue, store, holder


def _at(h: int, m: int, *, s: int = 0) -> datetime:
    # A fixed Monday (2026-06-08) so weekday-restricted crons are stable.
    return datetime(2026, 6, 8, h, m, s, tzinfo=UTC)


def _role_org() -> Organization:
    role = Role(
        name="QA Engineer",
        handle="qa",
        schedules=[Schedule(name="smoke", cron="0 9 * * *", task="run smoke tests")],
    )
    return Organization(name="Acme", roles=[role])


def _unit_org(target: str = "each") -> Organization:
    lead = Role(name="QA Lead", handle="qa-lead")
    dev = Role(name="QA Dev", handle="qa-dev")
    unit = OrgUnit(
        name="Quality",
        type="team",
        lead="QA Lead",
        roles=[lead, dev],
        schedules=[
            Schedule(name="standup", cron="0 9 * * *", task="standup", target=target)
        ],
    )
    return Organization(name="Acme", units=[unit])


# --- helpers count ---------------------------------------------------------


def test_has_and_count_schedules():
    assert has_schedules(_role_org())
    assert count_schedules(_unit_org()) == 1
    assert not has_schedules(Organization(name="Empty"))


# --- role schedule ---------------------------------------------------------


async def test_role_schedule_fires_to_own_inbox():
    scheduler, queue, _store, _ = _build(_role_org())
    # Window mode: pretend the previous tick was just before 09:00.
    scheduler._last_tick_utc = _at(8, 59, s=30)
    fired = await scheduler.tick_once(now=_at(9, 0, s=30))
    assert fired == 1
    tasks = queue.inbox_tasks()
    assert len(tasks) == 1
    topic, event = tasks[0]
    assert topic == "crewlet.agent.qa.inbox"
    assert event.payload["task_description"] == "run smoke tests"
    assert event.payload["scheduled"] is True
    assert event.payload["schedule_name"] == "smoke"
    assert event.payload["timeout_seconds"] == 180
    assert event.agent_id == "id-qa"
    # Lifecycle event emitted too.
    assert any(isinstance(e, ScheduledTaskFired) for _, e in queue.published)


async def test_dedup_across_reticks():
    scheduler, queue, _store, _ = _build(_role_org())
    scheduler._last_tick_utc = _at(8, 59, s=30)
    assert await scheduler.tick_once(now=_at(9, 0, s=30)) == 1
    # Re-evaluate the same fire minute: rewind last_tick so the 09:00 fire
    # is in-window again; the store claim must dedup it.
    scheduler._last_tick_utc = _at(8, 59, s=30)
    assert await scheduler.tick_once(now=_at(9, 0, s=30)) == 0
    assert len(queue.inbox_tasks()) == 1


async def test_fire_records_trace_and_shares_it_with_the_task():
    # Each fire is its own trace; the ledger row stores it and the
    # TaskAssigned carries it, so the turn joins the same trace (this is
    # what the dashboard "View calls" link follows).
    scheduler, queue, store, _ = _build(_role_org())
    scheduler._last_tick_utc = _at(8, 59, s=30)
    assert await scheduler.tick_once(now=_at(9, 0, s=30)) == 1
    rec = store.records[0]
    assert rec["outcome"] == "fired"
    assert rec["trace_id"]  # non-empty
    _topic, event = queue.inbox_tasks()[0]
    assert event.trace_id == rec["trace_id"]


async def test_each_member_runs_get_distinct_traces():
    scheduler, queue, _store, _ = _build(_unit_org("each"))
    scheduler._last_tick_utc = _at(8, 59, s=30)
    assert await scheduler.tick_once(now=_at(9, 0, s=30)) == 2
    traces = {event.trace_id for _, event in queue.inbox_tasks()}
    assert len(traces) == 2 and all(traces)  # one self-contained trace per fire


# --- unit delivery modes ---------------------------------------------------


async def test_unit_each_fans_out_to_every_direct_member():
    scheduler, queue, _store, _ = _build(_unit_org("each"))
    scheduler._last_tick_utc = _at(8, 59, s=30)
    fired = await scheduler.tick_once(now=_at(9, 0, s=30))
    assert fired == 2
    handles = {t for t, _ in queue.inbox_tasks()}
    assert handles == {
        "crewlet.agent.qa-lead.inbox",
        "crewlet.agent.qa-dev.inbox",
    }


async def test_unit_target_lead():
    scheduler, queue, _store, _ = _build(_unit_org("lead"))
    scheduler._last_tick_utc = _at(8, 59, s=30)
    fired = await scheduler.tick_once(now=_at(9, 0, s=30))
    assert fired == 1
    assert [t for t, _ in queue.inbox_tasks()] == ["crewlet.agent.qa-lead.inbox"]


async def test_each_member_failure_does_not_block_siblings():
    # Pool is missing 'qa-dev' (e.g. role not spawned); 'qa-lead' still fires.
    org = _unit_org("each")
    holder = [org]
    queue = _Queue()
    scheduler = Scheduler(
        event_queue=queue,
        agent_pool=_Pool([_Agent("qa-lead", "QA Lead", "id-qa-lead")]),
        org_provider=lambda: holder[0],
        store=MemoryScheduledRunStore(),
        default_timezone="UTC",
    )
    scheduler._last_tick_utc = _at(8, 59, s=30)
    fired = await scheduler.tick_once(now=_at(9, 0, s=30))
    assert fired == 1
    assert [t for t, _ in queue.inbox_tasks()] == ["crewlet.agent.qa-lead.inbox"]


async def test_disabled_schedule_does_not_fire():
    role = Role(
        name="QA",
        handle="qa",
        schedules=[Schedule(name="s", cron="0 9 * * *", task="x", enabled=False)],
    )
    scheduler, queue, _store, _ = _build(Organization(name="Acme", roles=[role]))
    scheduler._last_tick_utc = _at(8, 59, s=30)
    assert await scheduler.tick_once(now=_at(9, 0, s=30)) == 0
    assert queue.inbox_tasks() == []


# --- timezone --------------------------------------------------------------


async def test_dst_fallback_fires_once():
    # Europe/Amsterdam falls back 2026-10-25 03:00->02:00, so local 02:30
    # occurs at both 00:30 and 01:30 UTC. A '30 2 * * *' schedule must fire
    # once (deduped on the shared local fire-label), not twice.
    role = Role(
        name="QA",
        handle="qa",
        schedules=[
            Schedule(
                name="nightly",
                cron="30 2 * * *",
                task="x",
                timezone="Europe/Amsterdam",
            )
        ],
    )
    scheduler, queue, _store, _ = _build(Organization(name="Acme", roles=[role]))
    # First UTC instant (00:30Z).
    scheduler._last_tick_utc = datetime(2026, 10, 25, 0, 29, 30, tzinfo=UTC)
    assert (
        await scheduler.tick_once(now=datetime(2026, 10, 25, 0, 30, 30, tzinfo=UTC))
        == 1
    )
    # Second UTC instant (01:30Z) an hour later — same local 02:30 → deduped.
    scheduler._last_tick_utc = datetime(2026, 10, 25, 1, 29, 30, tzinfo=UTC)
    assert (
        await scheduler.tick_once(now=datetime(2026, 10, 25, 1, 30, 30, tzinfo=UTC))
        == 0
    )
    assert len(queue.inbox_tasks()) == 1


async def test_timezone_local_fire():
    role = Role(
        name="QA",
        handle="qa",
        schedules=[
            Schedule(
                name="ams",
                cron="30 9 * * *",
                task="x",
                timezone="Europe/Amsterdam",
            )
        ],
    )
    scheduler, queue, _store, _ = _build(Organization(name="Acme", roles=[role]))
    # 09:30 Amsterdam (CEST) == 07:30 UTC in June.
    scheduler._last_tick_utc = _at(7, 29, s=30)
    assert await scheduler.tick_once(now=_at(7, 30, s=30)) == 1
    # And it does NOT fire at 09:30 UTC.
    scheduler2, queue2, _s2, _ = _build(Organization(name="Acme", roles=[role]))
    scheduler2._last_tick_utc = _at(9, 29, s=30)
    assert await scheduler2.tick_once(now=_at(9, 30, s=30)) == 0


# --- catchup ---------------------------------------------------------------


async def test_catchup_fires_recent_missed_tick():
    scheduler, queue, _store, _ = _build(_role_org())
    # First tick (no prior last_tick) shortly after a missed 09:00 fire.
    fired = await scheduler.tick_once(now=_at(9, 2, s=0))
    assert fired == 1
    assert [t for t, _ in queue.inbox_tasks()] == ["crewlet.agent.qa.inbox"]


async def test_catchup_skips_when_outside_window():
    scheduler, queue, store, _ = _build(_role_org())
    # Daily schedule: catchup window clamps to 2h; 09:00 fire seen at 17:00
    # is far outside → skipped, not fired.
    fired = await scheduler.tick_once(now=_at(17, 0, s=0))
    assert fired == 0
    assert queue.inbox_tasks() == []
    assert any(r["outcome"] == "skipped_catchup" for r in store.records)


async def test_catchup_disabled_does_not_fire():
    role = Role(
        name="QA",
        handle="qa",
        schedules=[Schedule(name="s", cron="0 9 * * *", task="x", catchup=False)],
    )
    scheduler, queue, _store, _ = _build(Organization(name="Acme", roles=[role]))
    assert await scheduler.tick_once(now=_at(9, 2, s=0)) == 0
    assert queue.inbox_tasks() == []


# --- hot reload ------------------------------------------------------------


async def test_hot_reload_picks_up_new_schedule():
    empty = Organization(name="Acme", roles=[Role(name="QA", handle="qa")])
    holder = [empty]
    queue = _Queue()
    scheduler = Scheduler(
        event_queue=queue,
        agent_pool=_Pool([_Agent("qa", "QA", "id-qa")]),
        org_provider=lambda: holder[0],
        store=MemoryScheduledRunStore(),
        default_timezone="UTC",
    )
    scheduler._last_tick_utc = _at(8, 59, s=30)
    assert await scheduler.tick_once(now=_at(9, 0, s=30)) == 0

    # Operator hot-reloads: a schedule now exists for the same role.
    holder[0] = Organization(
        name="Acme",
        roles=[
            Role(
                name="QA",
                handle="qa",
                schedules=[Schedule(name="s", cron="0 9 * * *", task="x")],
            )
        ],
    )
    scheduler._last_tick_utc = _at(8, 59, s=30)
    assert await scheduler.tick_once(now=_at(9, 0, s=30)) == 1


# --- jitter ----------------------------------------------------------------


async def test_jitter_delays_firing_deterministically():
    org = _role_org()
    scheduler, queue, _store, _ = _build(org, jitter_seconds=45)
    j = scheduler._jitter_for("qa", "smoke")
    fire = _at(9, 0, s=0)

    # Just before the jittered instant → not yet due.
    scheduler._last_tick_utc = fire - timedelta(seconds=60)
    before = fire + timedelta(seconds=j) - timedelta(seconds=1)
    assert await scheduler.tick_once(now=before) == 0

    # At the jittered instant → fires.
    scheduler._last_tick_utc = before
    after = fire + timedelta(seconds=j)
    assert await scheduler.tick_once(now=after) == 1

    if j > 0:
        # With non-zero jitter, firing exactly on the minute is too early.
        s2, q2, _s, _ = _build(_role_org(), jitter_seconds=45)
        s2._last_tick_utc = fire - timedelta(seconds=60)
        assert await s2.tick_once(now=fire) == 0
