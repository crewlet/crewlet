"""Tests for the live token-meter reporter.

The reporter is the only way ``BudgetManager``'s counters leave the
engine process, and everything downstream -- the seat card's bar, the
agent header, the overview's org tile -- is drawn from what it says.
So the properties that matter are about not lying: report what the meter
holds, say which run it belongs to, and never publish faster than the
accounting can afford.
"""

from __future__ import annotations

import asyncio
from typing import Any

import pytest

from crewlet.budget_reporter import BudgetReporter
from crewlet.concurrency import BudgetManager
from crewlet.events.types import BudgetReported


class _Queue:
    """Records what was published, and on which topic."""

    def __init__(self) -> None:
        self.published: list[tuple[str, Any]] = []

    async def publish(self, topic: str, event: Any) -> None:
        self.published.append((topic, event))


ROLES = {"a1": "Engineer", "a2": "Researcher"}


def _reporter(
    manager: BudgetManager | None, queue: _Queue, *, interval: float = 0.01
) -> BudgetReporter:
    return BudgetReporter(
        manager_of=lambda: manager,
        event_queue=queue,  # type: ignore[arg-type]
        role_of=lambda agent_id: ROLES.get(agent_id, ""),
        interval=interval,
    )


async def test_a_report_carries_the_meter_and_its_identity() -> None:
    manager = BudgetManager(org_budget=1000, agent_budgets={"a1": 100})
    queue = _Queue()
    await manager.consume("a1", 40)

    event = await _reporter(manager, queue).publish_once()

    assert isinstance(event, BudgetReported)
    assert event.meter_id == manager.meter_id
    assert event.org_used_tokens == 40
    assert event.org_max_tokens == 1000
    assert event.agents == [
        {
            "agent_id": "a1",
            "role": "Engineer",
            "used_tokens": 40,
            "max_tokens": 100,
            "refused_at": "",
        }
    ]
    assert queue.published[0][0] == "crewlet.events.budget_reported"


async def test_only_metered_seats_are_reported() -> None:
    """A seat with no cap has no meter, which is not a cap of zero.

    Reporting it with zeros would put an empty bar on a card that should
    have none at all.
    """
    manager = BudgetManager(org_budget=1000)
    await manager.consume("uncapped", 90)

    event = await _reporter(manager, _Queue()).publish_once()

    assert event is not None
    assert event.agents == []
    # The spend still reached the org meter.
    assert event.org_used_tokens == 90


async def test_a_refusal_rides_on_the_report() -> None:
    manager = BudgetManager(org_budget=10_000, agent_budgets={"a1": 100})
    await manager.consume("a1", 60)
    await manager.consume("a1", 60)  # refused

    event = await _reporter(manager, _Queue()).publish_once()

    assert event is not None
    assert event.agents[0]["refused_at"], "the cap biting was not reported"
    # And the counter is still short of the cap, which is exactly why a
    # consumer must not derive exhaustion from the ratio.
    assert event.agents[0]["used_tokens"] < event.agents[0]["max_tokens"]


async def test_seq_increases_so_a_late_report_can_be_dropped() -> None:
    manager = BudgetManager(org_budget=100)
    reporter = _reporter(manager, _Queue())
    first = await reporter.publish_once()
    second = await reporter.publish_once()
    assert first is not None and second is not None
    assert second.seq > first.seq


async def test_nothing_is_published_without_a_manager() -> None:
    queue = _Queue()
    assert await _reporter(None, queue).publish_once() is None
    assert queue.published == []


async def test_many_charges_collapse_into_one_report() -> None:
    """The hook fires per round, under the accounting lock.

    Publishing from the hook would put broker work inside the critical
    section; the tick is what keeps that off the engine's hot path.
    """
    manager = BudgetManager(org_budget=10**9)
    queue = _Queue()
    reporter = _reporter(manager, queue, interval=0.05)
    await reporter.start()
    try:
        for _ in range(50):
            await manager.consume("a1", 10)
        await asyncio.sleep(0.12)
    finally:
        await reporter.stop()

    assert 1 <= len(queue.published) <= 3, (
        f"50 charges produced {len(queue.published)} reports"
    )
    assert queue.published[-1][1].org_used_tokens == 500


async def test_a_quiet_meter_publishes_nothing() -> None:
    manager = BudgetManager(org_budget=100)
    queue = _Queue()
    reporter = _reporter(manager, queue, interval=0.02)
    await reporter.start()
    # `start` attaches, which marks the freshly-visible figures dirty --
    # that first report is the point. What must NOT happen is a stream
    # of identical ones while nothing is being charged.
    await asyncio.sleep(0.1)
    await reporter.stop()
    assert len(queue.published) == 1


async def test_the_reporter_follows_a_replaced_manager() -> None:
    """The engine swaps the manager wholesale on first activation.

    A captured reference would leave the reporter publishing a meter
    nobody charges any more -- a figure frozen forever with no error.
    """
    current = BudgetManager(org_budget=100)
    queue = _Queue()
    reporter = BudgetReporter(
        manager_of=lambda: current,
        event_queue=queue,  # type: ignore[arg-type]
        role_of=lambda _agent_id: "",
    )
    reporter.attach()

    replacement = BudgetManager(org_budget=999)
    current = replacement
    reporter.attach()
    await current.consume("a1", 7)

    event = await reporter.publish_once()
    assert event is not None
    assert event.meter_id == replacement.meter_id
    assert event.org_max_tokens == 999


async def test_a_publish_failure_never_stops_the_tick() -> None:
    class _Broken(_Queue):
        async def publish(self, topic: str, event: Any) -> None:
            raise RuntimeError("broker down")

    manager = BudgetManager(org_budget=100)
    reporter = _reporter(manager, _Broken(), interval=0.02)
    await reporter.start()
    await asyncio.sleep(0.08)
    await manager.consume("a1", 5)
    await asyncio.sleep(0.05)
    # Still running, still accounting.
    assert manager.org_budget.used_tokens == 5
    await reporter.stop()


async def test_stop_detaches_the_hook() -> None:
    manager = BudgetManager(org_budget=100)
    reporter = _reporter(manager, _Queue())
    await reporter.start()
    await reporter.stop()
    # No hook left behind on a manager the engine may keep using.
    assert manager._on_change is None


if __name__ == "__main__":  # pragma: no cover
    pytest.main([__file__, "-v"])
